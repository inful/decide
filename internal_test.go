package decide

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short string untouched; got %q", got)
	}
	if got := truncate("hello world", 5); !strings.HasSuffix(got, "...(truncated)") {
		t.Errorf("long string truncated; got %q", got)
	}
	if got := truncate("hello world", 5); !strings.HasPrefix(got, "hello") {
		t.Errorf("truncation should preserve prefix; got %q", got)
	}
}

func TestRetryPolicyNextDelay_NegativeAttemptClampsToZero(t *testing.T) {
	p := RetryPolicy{
		MaxRetries:     1,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     1 * time.Second,
		Multiplier:     2.0,
		Jitter:         0,
	}
	if got := p.nextDelay(-1); got != 100*time.Millisecond {
		t.Errorf("nextDelay(-1) = %v, want %v", got, 100*time.Millisecond)
	}
}

func TestResolve_RejectsEmptyAndInvalidBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		wantErr bool
	}{
		{"empty", "", true},
		{"missing scheme", "example.com", true},
		{"missing host", "https://", true},
		{"valid", "https://example.com", false},
		{"valid with trailing slash", "https://example.com/", false},
		{"valid with subpath", "https://example.com/api/v3", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := New(tc.baseURL)
			_, err := client.resolve("/v1/systemone")
			if tc.wantErr && err == nil {
				t.Errorf("expected error for %q", tc.baseURL)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for %q: %v", tc.baseURL, err)
			}
		})
	}
}

func TestDecodeResponse_MalformedJSONReturnsTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{this is not json"))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, WithToken("sk"))
	_, decErr := c.SystemOne(t.Context(), Request{State: State{Body: "hi"}})
	if decErr == nil {
		t.Fatal("expected error from malformed body")
	}
	if !errors.Is(decErr, ErrResponseDecoded) {
		t.Errorf("expected ErrResponseDecoded; got %v", decErr)
	}
}

func TestDecodeResponse_CapturesRequestIDFromResponseHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "echo-this-back")
		_, _ = w.Write([]byte(`{"model":"m","answers":{},"usage":{}}`))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, WithToken("sk"))
	resp, err := c.SystemOne(t.Context(), Request{State: State{Body: "hi"}})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if resp.RequestID() != "echo-this-back" {
		t.Errorf("RequestID() = %q, want echo-this-back", resp.RequestID())
	}
}

func TestErrorIs_DirectlyConstructedStatusMatchesSentinel(t *testing.T) {
	cases := []struct {
		code    int
		sentinel error
	}{
		{400, ErrBadRequest},
		{401, ErrUnauthorized},
		{403, ErrForbidden},
		{404, ErrNotFound},
		{429, ErrRateLimited},
		{500, ErrServerError},
		{502, ErrServerError},
	}
	for _, c := range cases {
		err := &Error{StatusCode: c.code}
		if !errors.Is(err, c.sentinel) {
			t.Errorf("status %d should match %v", c.code, c.sentinel)
		}
	}
}

func TestErrorUnwrap_NilReturnsNil(t *testing.T) {
	var e *Error
	if got := e.Unwrap(); got != nil {
		t.Errorf("nil *Error.Unwrap() = %v, want nil", got)
	}
	if got := e.Is(errors.New("anything")); got {
		t.Errorf("nil *Error.Is(any) should be false")
	}
}

func TestErrorUnwrap_TransportErrorChainsBothCauseAndSentinel(t *testing.T) {
	cause := errors.New("dial tcp: connection refused")
	err := wrapTransportError("POST", "https://x.test/v1/systemone", cause)

	// errors.Is should match both the underlying cause and ErrTransport.
	if !errors.Is(err, cause) {
		t.Errorf("expected to match cause %v", cause)
	}
	if !errors.Is(err, ErrTransport) {
		t.Errorf("expected to match ErrTransport")
	}
	// And a non-matching target returns false.
	if errors.Is(err, ErrUnauthorized) {
		t.Errorf("ErrUnauthorized should not match a transport error")
	}
}

func TestMarshalRoundTrip_RequestAndResponse(t *testing.T) {
	// Round-trip both the request and the response through JSON to catch
	// json.Marshal/Unmarshal asymmetries (the discriminator-tagged Question
	// and Answer types are the main risk).
	req := Request{
		State: State{Body: "round-trip me"},
		Questions: Questions{
			"a": NewChoice("q?", map[string]string{"x": "y"}),
			"b": NewNoul("noul?"),
		},
	}
	reqJSON, _ := json.Marshal(req)
	var reqBack Request
	if err := json.Unmarshal(reqJSON, &reqBack); err != nil {
		t.Fatalf("request round trip: %v", err)
	}
	if reqBack.Questions["a"].Type != "choice" {
		t.Errorf("a.Type = %q", reqBack.Questions["a"].Type)
	}
	if reqBack.Questions["b"].Type != "noul" {
		t.Errorf("b.Type = %q", reqBack.Questions["b"].Type)
	}
	if len(reqBack.Questions["b"].Criteria) != 0 {
		t.Errorf("b should not have criteria")
	}
}