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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/settings"
)

// arrTestTimeout bounds a connection-test ping so a black-holed instance cannot
// pin a request handler.
const arrTestTimeout = 10 * time.Second

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

	if idx < 0 && req.APIKey == "" {
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
	conns.Instances = append(conns.Instances[:idx], conns.Instances[idx+1:]...)
	if err := s.saveArrConnections(r.Context(), conns); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete arr connection")
		return
	}
	if err := s.secrets.Delete(arrSecretKey(kind)); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete arr credentials")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

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
	conns, err := s.loadArrConnections(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read arr connections")
		return
	}
	var baseURL string
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
	apiKey, err := s.secrets.Get(arrSecretKey(kind))
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "api_key is not configured")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not read arr credentials")
		return
	}

	var client arr.Client
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
		writeError(w, http.StatusBadGateway, arrTestError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "kind": kind})
}

// arrTestError maps a client failure to an operator-facing message that carries
// no credentials (the clients scrub keys already; the messages stay generic).
func arrTestError(err error) string {
	switch {
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

// normalizeArrBaseURL validates and canonicalizes a connection base URL.
func normalizeArrBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("base_url must be an absolute http(s) URL")
	}
	return strings.TrimRight(raw, "/"), nil
}
