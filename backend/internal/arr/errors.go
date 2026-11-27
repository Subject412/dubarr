package arr

import (
	"errors"
	"fmt"
)

// Sentinel errors classify client failures. Callers should test with
// errors.Is; a *StatusError also matches ErrUnexpectedStatus and an *AuthError
// also matches ErrAuth.
var (
	// ErrAuth means the instance rejected the API key (HTTP 401/403).
	ErrAuth = errors.New("arr: authentication failed")
	// ErrNetwork means the request never produced an HTTP response: DNS,
	// connection, TLS, timeout or cancellation.
	ErrNetwork = errors.New("arr: network error")
	// ErrUnexpectedStatus means the instance answered with an HTTP status the
	// client does not handle (anything other than 2xx, 401 or 403).
	ErrUnexpectedStatus = errors.New("arr: unexpected HTTP status")
	// ErrDecode means the instance answered 2xx with a body that is not the
	// JSON shape the client expected.
	ErrDecode = errors.New("arr: decode response")
)

// AuthError reports a rejected API key. Unlike the sentinel alone it carries
// the HTTP status code structurally, so callers can tell 401 from 403 without
// parsing Error() text. It never contains credentials or a response body.
type AuthError struct {
	// Code is the HTTP status code (401 or 403).
	Code int
	// Method and Path identify the failed request.
	Method string
	Path   string
}

// Error implements error.
func (e *AuthError) Error() string {
	return fmt.Sprintf("%s: %s %s: HTTP %d", ErrAuth, e.Method, e.Path, e.Code)
}

// Is reports whether target is ErrAuth.
func (e *AuthError) Is(target error) bool { return target == ErrAuth }

// StatusError carries the HTTP status code of an unrecognised response.
// It never contains response bodies or credentials.
type StatusError struct {
	// Code is the HTTP status code.
	Code int
	// Method and Path identify the failed request.
	Method string
	Path   string
}

// Error implements error.
func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %d from %s %s", ErrUnexpectedStatus, e.Code, e.Method, e.Path)
}

// Is reports whether target is ErrUnexpectedStatus.
func (e *StatusError) Is(target error) bool { return target == ErrUnexpectedStatus }

// APIVersionError reports an instance that does not serve the API version
// dubarr requires. Dubarr speaks only the newest API each product ships
// (Sonarr 5, Radarr 3) and never probes for an older one, so this failure is
// deliberately loud and specific: it names the product, the version expected
// and the path tried. It carries no response body and no credential.
type APIVersionError struct {
	// Kind is the product dubarr was talking to.
	Kind Kind
	// APIBase is the base path requested, e.g. "/api/v5".
	APIBase string
	// Code is the HTTP status the instance answered with (404).
	Code int
	// Err is the status error this was derived from, so the failure still
	// classifies as ErrUnexpectedStatus for callers that only know the
	// generic class.
	Err *StatusError
}

// Error implements error.
func (e *APIVersionError) Error() string {
	product, version := requiredAPI(e.Kind)
	return fmt.Sprintf("%s: this instance does not serve %s (%d) — dubarr %s requires %s %s",
		e.Kind, e.APIBase, e.Code, pinnedRelease, product, version)
}

// Unwrap exposes the underlying *StatusError.
func (e *APIVersionError) Unwrap() error {
	if e.Err == nil {
		return nil
	}
	return e.Err
}

// requiredAPI names the product and the API release line dubarr requires from
// it. The constructors pick the base path; this maps the same choice onto the
// words an operator reads in the connection test.
func requiredAPI(kind Kind) (product, version string) {
	if kind == KindSonarr {
		return "Sonarr", "5.x"
	}
	return "Radarr", "3.x"
}
