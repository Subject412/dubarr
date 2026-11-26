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
