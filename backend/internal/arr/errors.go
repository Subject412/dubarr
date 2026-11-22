package arr

import (
	"errors"
	"fmt"
)

// Sentinel errors classify client failures. Callers should test with
// errors.Is; a *StatusError also matches ErrUnexpectedStatus.
var (
	// ErrAuth means the instance rejected the API key (HTTP 401/403).
	ErrAuth = errors.New("arr: authentication failed")
	// ErrNetwork means the request never produced an HTTP response: DNS,
	// connection, TLS, timeout or cancellation.
	ErrNetwork = errors.New("arr: network error")
	// ErrUnexpectedStatus means the instance answered with an HTTP status the
	// client does not handle (anything other than 2xx, 401 or 403).
	ErrUnexpectedStatus = errors.New("arr: unexpected HTTP status")
)

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
