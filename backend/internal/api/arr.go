// Arr connection settings: list/upsert/delete Radarr and Sonarr instances and
// run a connection test with the M2-1 clients.
//
// Connection metadata (kind, base_url, enabled) lives in the arr.connections
// settings section as JSON. The API key never does: it is written only through
// internal/secrets (AES-256-GCM) under arr.<kind>.api_key, and read paths expose
// just secrets.Mask(key) (eight stars plus the last four characters).

package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/settings"
)

// arrTestTimeout bounds a connection-test ping so a black-holed instance cannot
// pin a request handler.
const arrTestTimeout = 10 * time.Second

// arrSecretStore is the subset of secrets.Service the arr handlers use. Keeping
// it an interface lets tests inject failures at the delete/set seam so the
// ordering guarantees below stay pinned by tests.
type arrSecretStore interface {
	Set(key, plaintext string) error
	Get(key string) (string, error)
	Delete(key string) error
}

// arrConnectionsMu serializes the read-modify-write of the arr.connections
// section. Handlers load the section, mutate their instance and save it back;
// without a lock two concurrent upserts (or an upsert racing a delete) would
// both write a stale slice and the later write would drop the other change.
var arrConnectionsMu sync.Mutex

// arrConnectionView is a connection as served by the API: the key is masked.
type arrConnectionView struct {
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	Enabled bool   `json:"enabled"`
	APIKey  string `json:"api_key"`
}

type arrConnectionsResponse struct {
	Instances []arrConnectionView `json:"instances"`
}

// arrUpsertRequest is the write shape. APIKey is write-only: an empty value on
// update keeps the stored key, and on create it is rejected.
type arrUpsertRequest struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Enabled *bool  `json:"enabled"`
}

// arrTestRequest is the optional body of POST /api/settings/arr/{kind}/test.
// Both fields are drafts: an absent or blank field falls back to the saved
// connection value, so an empty body keeps the "test what is saved" behaviour.
// Nothing here is ever persisted.
type arrTestRequest struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// arrSecretKey names the encrypted secret for one instance kind.
func arrSecretKey(kind string) string { return "arr." + kind + ".api_key" }

// validArrKind reports whether kind is a supported *arr application.
func validArrKind(kind string) bool { return kind == "radarr" || kind == "sonarr" }

// arrReady reports whether the secret service needed to store and read keys is
// wired; handlers fail closed with 503 otherwise.
func (s *Service) arrReady() bool { return s.store != nil && s.secrets != nil }

func (s *Service) loadArrConnections(ctx context.Context) (settings.ArrConnections, error) {
	var conns settings.ArrConnections
	raw, ok, err := s.store.Get(ctx, settings.KeyArrConnections)
	if err != nil {
		return conns, err
	}
	if !ok {
		return conns, nil
	}
	if err := json.Unmarshal(raw, &conns); err != nil {
		return conns, fmt.Errorf("api: decode %s: %w", settings.KeyArrConnections, err)
	}
	if conns.Instances == nil {
		conns.Instances = []settings.ArrConnection{}
	}
	return conns, nil
}

// saveArrConnections validates and persists the section through the settings
// store, so the arr.connections contract stays in one place.
func (s *Service) saveArrConnections(ctx context.Context, conns settings.ArrConnections) error {
	blob, err := json.Marshal(conns)
	if err != nil {
		return err
	}
	return s.store.Set(ctx, settings.KeyArrConnections, blob)
}

// maskedKey decrypts the stored key only to derive its display form. It never
// returns key material.
func (s *Service) maskedKey(kind string) (string, error) {
	key, err := s.secrets.Get(arrSecretKey(kind))
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return secrets.Mask(key), nil
}

func (s *Service) handleListArr(w http.ResponseWriter, r *http.Request) {
	if !s.arrReady() {
		writeError(w, http.StatusServiceUnavailable, "arr settings are not configured")
		return
	}
	conns, err := s.loadArrConnections(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read arr connections")
		return
	}
	out := arrConnectionsResponse{Instances: make([]arrConnectionView, 0, len(conns.Instances))}
	for _, c := range conns.Instances {
		masked, err := s.maskedKey(c.Kind)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not read arr credentials")
			return
		}
		out.Instances = append(out.Instances, arrConnectionView{
			Kind: c.Kind, BaseURL: c.BaseURL, Enabled: c.Enabled, APIKey: masked,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleUpsertArr(w http.ResponseWriter, r *http.Request) {
	if !s.arrReady() {
		writeError(w, http.StatusServiceUnavailable, "arr settings are not configured")
		return
	}
	kind := strings.ToLower(strings.TrimSpace(r.PathValue("kind")))
	if !validArrKind(kind) {
		writeError(w, http.StatusNotFound, "unknown arr kind")
		return
	}
	var req arrUpsertRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	baseURL, err := normalizeArrBaseURL(req.BaseURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Serialize the load -> mutate -> save sequence; see arrConnectionsMu.
	arrConnectionsMu.Lock()
	defer arrConnectionsMu.Unlock()

	conns, err := s.loadArrConnections(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read arr connections")
		return
	}
	idx := -1
	for i, c := range conns.Instances {
		if c.Kind == kind {
			idx = i
			break
		}
	}

	created := idx < 0
	if created && req.APIKey == "" {
		writeError(w, http.StatusBadRequest, "api_key is required when adding a connection")
		return
	}

	enabled := true
	if idx >= 0 {
		enabled = conns.Instances[idx].Enabled
	}
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	// Write the secret first: a secret without metadata is an invisible orphan
	// that a retry overwrites, whereas metadata without a key would list a
	// connection that can never be tested.
	if req.APIKey != "" {
		if err := s.secrets.Set(arrSecretKey(kind), req.APIKey); err != nil {
			writeError(w, http.StatusInternalServerError, "could not store arr credentials")
			return
		}
	}

	conn := settings.ArrConnection{Kind: kind, BaseURL: baseURL, Enabled: enabled}
	if idx >= 0 {
		conns.Instances[idx] = conn
	} else {
		conns.Instances = append(conns.Instances, conn)
	}
	if err := s.saveArrConnections(r.Context(), conns); err != nil {
		// Compensate for the secret-first write on a failed create: nothing in
		// the section will point at that key, so drop it instead of leaving an
		// orphan behind. On an update the key belongs to the existing
		// connection, so deleting it would break a working instance.
		if created && req.APIKey != "" {
			_ = s.secrets.Delete(arrSecretKey(kind))
		}
		writeError(w, http.StatusInternalServerError, "could not save arr connection")
		return
	}

	masked, err := s.maskedKey(kind)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read arr credentials")
		return
	}
	writeJSON(w, http.StatusOK, arrConnectionView{
		Kind: conn.Kind, BaseURL: conn.BaseURL, Enabled: conn.Enabled, APIKey: masked,
	})
}

func (s *Service) handleDeleteArr(w http.ResponseWriter, r *http.Request) {
	if !s.arrReady() {
		writeError(w, http.StatusServiceUnavailable, "arr settings are not configured")
		return
	}
	kind := strings.ToLower(strings.TrimSpace(r.PathValue("kind")))
	if !validArrKind(kind) {
		writeError(w, http.StatusNotFound, "unknown arr kind")
		return
	}
	// Serialize with upsert so a delete cannot resurrect a concurrently
	// updated section.
	arrConnectionsMu.Lock()
	defer arrConnectionsMu.Unlock()

	conns, err := s.loadArrConnections(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read arr connections")
		return
	}
	idx := -1
	for i, c := range conns.Instances {
		if c.Kind == kind {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeError(w, http.StatusNotFound, "arr connection not found")
		return
	}
	// Remove the secret first. Delete is idempotent, so a retry after an
	// interrupted attempt succeeds; dropping the key before the metadata means
	// a later metadata failure leaves a connection that can be re-keyed, never
	// orphaned key material with no listing to clean it up from.
	if err := s.secrets.Delete(arrSecretKey(kind)); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete arr credentials")
		return
	}
	conns.Instances = append(conns.Instances[:idx], conns.Instances[idx+1:]...)
	if err := s.saveArrConnections(r.Context(), conns); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete arr connection")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestArr pings an *arr instance. The optional JSON body
// ({base_url?, api_key?}) carries the values typed in the form so an operator
// can test before saving; absent or blank fields fall back to the saved
// connection and its stored key. Nothing is persisted: the section and the
// secret are read-only here, so a failing draft test leaves both untouched.
func (s *Service) handleTestArr(w http.ResponseWriter, r *http.Request) {
	if !s.arrReady() {
		writeError(w, http.StatusServiceUnavailable, "arr settings are not configured")
		return
	}
	kind := strings.ToLower(strings.TrimSpace(r.PathValue("kind")))
	if !validArrKind(kind) {
		writeError(w, http.StatusNotFound, "unknown arr kind")
		return
	}

	// The body is optional and carries drafts only: the operator may test the
	// values typed in the form before saving them. An empty body (or `{}`)
	// keeps the original behaviour of testing the saved connection.
	var req arrTestRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// A draft base URL is validated exactly like an upsert, so a typo is a 400
	// on both paths instead of a 502 from the client constructor.
	var baseURL string
	if draft := strings.TrimSpace(req.BaseURL); draft != "" {
		normalized, err := normalizeArrBaseURL(draft)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		baseURL = normalized
	} else {
		conns, err := s.loadArrConnections(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not read arr connections")
			return
		}
		for _, c := range conns.Instances {
			if c.Kind == kind {
				baseURL = c.BaseURL
				break
			}
		}
		if baseURL == "" {
			writeError(w, http.StatusNotFound, "arr connection not found")
			return
		}
	}

	// A blank (or whitespace-only) draft key means "use the stored one"; neither
	// branch writes. The key is trimmed so `"   "` cannot override the stored
	// secret with an empty X-Api-Key.
	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" {
		stored, err := s.secrets.Get(arrSecretKey(kind))
		if err != nil {
			if errors.Is(err, secrets.ErrNotFound) {
				writeError(w, http.StatusBadRequest, "api_key is not configured")
				return
			}
			writeError(w, http.StatusInternalServerError, "could not read arr credentials")
			return
		}
		apiKey = stored
	}

	var (
		client arr.Client
		err    error
	)
	switch kind {
	case "radarr":
		client, err = arr.NewRadarrClient(baseURL, apiKey, arrTestTimeout)
	case "sonarr":
		client, err = arr.NewSonarrClient(baseURL, apiKey, arrTestTimeout)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid arr connection")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), arrTestTimeout)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		class, statusCode, host, port := arrTestDiagnostics(err, baseURL)
		writeJSON(w, http.StatusBadGateway, arrTestFailure{
			Error:      arrTestError(err),
			Class:      class,
			StatusCode: statusCode,
			Host:       host,
			Port:       port,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "kind": kind})
}

// arrTestFailure is the 502 body of a failed connection test. Error keeps the
// pre-existing operator message verbatim; Class/StatusCode/Host/Port are the
// additive diagnostics. The shape carries no credentials and no response-body
// excerpt: the API key never reaches this struct.
type arrTestFailure struct {
	Error      string `json:"error"`
	Class      string `json:"class"`
	StatusCode int    `json:"status_code"`
	Host       string `json:"host"`
	Port       string `json:"port"`
}

// arrTestError maps a client failure to an operator-facing message that carries
// no credentials (the clients scrub keys already; the messages stay generic).
// The one deliberate exception is the API-version failure: it names the
// product, the version dubarr requires and the path tried, because "wrong API
// version" and "unexpected status 404" are different problems and only the
// specific text tells the operator which one they have.
func arrTestError(err error) string {
	var versionErr *arr.APIVersionError
	switch {
	case errors.As(err, &versionErr):
		return versionErr.Error()
	case errors.Is(err, arr.ErrAuth):
		return "authentication failed: the API key was rejected"
	case errors.Is(err, arr.ErrNetwork):
		return "could not reach the instance: check the base URL and network"
	case errors.Is(err, arr.ErrUnexpectedStatus):
		return "the instance returned an unexpected HTTP status"
	default:
		return "connection test failed"
	}
}

// arrTestDiagnostics classifies a failed ping into the additive diagnostics
// payload. It returns the failure class (see the arrFailure* constants), the
// HTTP status code when a response was received (0 otherwise), and the target
// host/port parsed from the normalised base URL the ping used (empty when the
// URL does not parse).
//
// It never includes error text, so no credential or response body can leak
// through it; the client already scrubs the API key from its own messages.
func arrTestDiagnostics(err error, baseURL string) (class string, statusCode int, host, port string) {
	host, port = arrTargetHostPort(baseURL)

	var (
		authErr   *arr.AuthError
		statusErr *arr.StatusError
		baseErr   *arrBaseURLError
	)
	switch {
	case errors.As(err, &authErr):
		return arrFailureAuth, authErr.Code, host, port
	case errors.As(err, &statusErr):
		return arrFailureStatus, statusErr.Code, host, port
	case errors.Is(err, arr.ErrKindMismatch):
		return arrFailureKindMismatch, 0, host, port
	case errors.As(err, &baseErr):
		return arrFailureInvalid, 0, host, port
	case errors.Is(err, arr.ErrDecode):
		return arrFailureDecode, 0, host, port
	case isDNSError(err):
		// A resolver failure is the more useful diagnosis even when it
		// self-reports a timeout (IsTimeout) or a missing record (IsNotFound);
		// checking the deadline cases first would hide which part failed.
		return arrFailureDNS, 0, host, port
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		// A timeout is also an ErrNetwork; it is the more useful diagnosis.
		return arrFailureTimeout, 0, host, port
	case isNetTimeout(err):
		// Socket-level deadlines (dial/read/write) surface as a net.Error
		// reporting Timeout() without either deadline sentinel.
		return arrFailureTimeout, 0, host, port
	case isTLSError(err):
		return arrFailureTLS, 0, host, port
	case errors.Is(err, arr.ErrNetwork):
		return arrFailureNetwork, 0, host, port
	default:
		// Residual transport failure: still unreachable, and still not a
		// credential-bearing message.
		return arrFailureNetwork, 0, host, port
	}
}

// Failure classes reported in the 502 payload. "network" is the documented
// fallback for a transport failure that matches none of the narrower classes.
const (
	arrFailureAuth         = "auth"
	arrFailureTimeout      = "timeout"
	arrFailureDNS          = "dns"
	arrFailureTLS          = "tls"
	arrFailureStatus       = "status"
	arrFailureKindMismatch = "kind_mismatch"
	arrFailureDecode       = "decode"
	arrFailureInvalid      = "invalid"
	arrFailureNetwork      = "network"
)

// arrTargetHostPort splits the ping target into host and port, defaulting the
// port by scheme when the URL omits it. Both are empty when baseURL does not
// parse or has no host. Userinfo is never part of Hostname().
func arrTargetHostPort(baseURL string) (host, port string) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return "", ""
	}
	host = u.Hostname()
	port = u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return host, port
}

// isDNSError reports whether a *net.DNSError is anywhere in the chain.
func isDNSError(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

// isNetTimeout reports whether a net.Error in the chain reports Timeout(). It
// catches socket-level deadlines (dial/read/write) that carry neither
// context.DeadlineExceeded nor os.ErrDeadlineExceeded. A *net.DNSError also
// satisfies net.Error, but isDNSError is checked first.
func isNetTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// isTLSError reports whether a certificate or TLS handshake failure is anywhere
// in the chain. Types are matched structurally so no message text is needed.
func isTLSError(err error) bool {
	var (
		unknownAuthority x509.UnknownAuthorityError
		hostname         x509.HostnameError
		certInvalid      x509.CertificateInvalidError
		recordHeader     tls.RecordHeaderError
		verify           *tls.CertificateVerificationError
	)
	return errors.As(err, &unknownAuthority) ||
		errors.As(err, &hostname) ||
		errors.As(err, &certInvalid) ||
		errors.As(err, &recordHeader) ||
		errors.As(err, &verify)
}

// normalizeArrBaseURL validates and canonicalizes a connection base URL.
// Credentials embedded in the URL are rejected: the API key belongs in its own
// secret, never in a URL that ends up in settings, logs and error text.
func normalizeArrBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", &arrBaseURLError{msg: "base_url must be an absolute http(s) URL"}
	}
	if u.User != nil {
		return "", &arrBaseURLError{msg: "base_url must not contain credentials"}
	}
	return strings.TrimRight(raw, "/"), nil
}

// arrBaseURLError is a base-URL validation failure. It is a 400 at the HTTP
// layer and the "invalid" diagnostics class; msg is the operator-facing text.
type arrBaseURLError struct{ msg string }

func (e *arrBaseURLError) Error() string { return e.msg }
