package decide

import (
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors so callers can branch on categories with errors.Is.
// They are unwrapped from *Error (and any wrapping transport error).
var (
	ErrBadRequest      = errors.New("decide: bad request")
	ErrUnauthorized    = errors.New("decide: unauthorized")
	ErrForbidden       = errors.New("decide: forbidden")
	ErrNotFound       = errors.New("decide: not found")
	ErrRateLimited     = errors.New("decide: rate limited")
	ErrServerError     = errors.New("decide: server error")
	ErrTransport       = errors.New("decide: transport error")
	ErrResponseDecoded = errors.New("decide: response decode error")
)

// Error is the concrete error type returned by Client calls. It carries the
// HTTP method/URL/status and the response body (truncated to ~512 bytes in
// the formatted message) so callers can log or inspect failure context.
type Error struct {
	StatusCode int
	Status     string
	Method     string
	URL        string
	Body       string

	// sentinel is the package-level Err* matching the status code.
	sentinel error
	// cause is the underlying error for transport-level failures.
	cause error
}

func (e *Error) Error() string {
	body := e.Body
	if len(body) > 512 {
		body = body[:512] + "...(truncated)"
	}
	if e.StatusCode == 0 {
		return fmt.Sprintf("decide: %s %s: %s: %s", e.Method, e.URL, e.Status, body)
	}
	return fmt.Sprintf("decide: %s %s: %d %s: %s", e.Method, e.URL, e.StatusCode, e.Status, body)
}

// Unwrap exposes both the package sentinel and any underlying cause so that
// errors.Is can match callers' preferred granularity.
func (e *Error) Unwrap() []error {
	if e == nil {
		return nil
	}
	var out []error
	if e.cause != nil {
		out = append(out, e.cause)
	}
	if e.sentinel != nil {
		out = append(out, e.sentinel)
	}
	return out
}

// Is lets errors.Is match a *Error against the package sentinels even when the
// caller constructed the value directly (without going through newHTTPError).
// Without this, *Error{StatusCode: 401} would not match ErrUnauthorized.
func (e *Error) Is(target error) bool {
	if e == nil || target == nil {
		return false
	}
	if e.sentinel != nil && errors.Is(e.sentinel, target) {
		return true
	}
	if expected := sentinelForStatus(e.StatusCode); expected != nil && errors.Is(expected, target) {
		return true
	}
	return false
}

// IsRetryable reports whether the error represents a transient failure that
// the client's retry policy might recover from.
func (e *Error) IsRetryable() bool {
	if e == nil {
		return false
	}
	if e.StatusCode == 0 {
		// Transport errors are retryable; context errors are filtered earlier.
		return true
	}
	return isRetryableStatus(e.StatusCode)
}

// newHTTPError wraps an HTTP response with the request context.
func newHTTPError(method, url string, resp *http.Response, body []byte) *Error {
	return &Error{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Method:     method,
		URL:        url,
		Body:       string(body),
		sentinel:   sentinelForStatus(resp.StatusCode),
	}
}

// wrapTransportError wraps a transport-level error (DNS, dial, TLS, etc.) as
// a retryable ErrTransport while preserving the underlying cause.
func wrapTransportError(method, url string, err error) *Error {
	return &Error{
		StatusCode: 0,
		Status:     "transport error",
		Method:     method,
		URL:        url,
		Body:       err.Error(),
		sentinel:   ErrTransport,
		cause:      err,
	}
}

func sentinelForStatus(code int) error {
	switch {
	case code == http.StatusBadRequest:
		return ErrBadRequest
	case code == http.StatusUnauthorized:
		return ErrUnauthorized
	case code == http.StatusForbidden:
		return ErrForbidden
	case code == http.StatusNotFound:
		return ErrNotFound
	case code == http.StatusTooManyRequests:
		return ErrRateLimited
	case code >= 500:
		return ErrServerError
	}
	return nil
}