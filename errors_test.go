package decide

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorError(t *testing.T) {
	err := &Error{
		StatusCode: 503,
		Status:     "Service Unavailable",
		Method:     "POST",
		URL:        "https://example.test/v1/systemone",
		Body:       "upstream busy",
	}
	want := `decide: POST https://example.test/v1/systemone: 503 Service Unavailable: upstream busy`
	if got := err.Error(); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestErrorError_TruncatesLongBody(t *testing.T) {
	long := make([]byte, 0, 4096)
	for i := 0; i < 4096; i++ {
		long = append(long, 'x')
	}
	err := &Error{
		StatusCode: 500,
		Status:     "Internal Server Error",
		Body:       string(long),
	}
	msg := err.Error()
	if len(msg) > 800 {
		t.Errorf("error message length %d, expected truncation below 800", len(msg))
	}
}

func TestErrorIsRetryable(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{200, false},
		{201, false},
		{204, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{422, false},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, true},
		{501, false}, // not in our retryable set
		{505, false},
	}
	for _, c := range cases {
		err := &Error{StatusCode: c.code}
		if got := err.IsRetryable(); got != c.want {
			t.Errorf("status %d IsRetryable = %v, want %v", c.code, got, c.want)
		}
	}
}

func TestErrorIs(t *testing.T) {
	// We want callers to be able to use errors.Is(err, decide.ErrUnauthorized) etc.
	base := &Error{StatusCode: 401}
	if !errors.Is(base, ErrUnauthorized) {
		t.Errorf("401 should match ErrUnauthorized")
	}
	if errors.Is(base, ErrRateLimited) {
		t.Errorf("401 should not match ErrRateLimited")
	}
}

func TestWrapHTTPError(t *testing.T) {
	err := fmt.Errorf("dial tcp: connection refused")
	wrapped := wrapTransportError("POST", "https://example.test/v1/systemone", err)
	if wrapped == nil {
		t.Fatalf("wrapTransportError returned nil")
	}
	if !errors.Is(wrapped, err) {
		t.Errorf("wrapped error should unwrap to the original transport error")
	}
	if !wrapped.IsRetryable() {
		t.Errorf("network error should be retryable")
	}
}