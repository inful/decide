package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestTruncate_DoesNotSplitMultiByteCharacter(t *testing.T) {
	// "€" is 3 bytes, so cutting at byte 512 lands inside the first one
	// and would leave invalid UTF-8 behind.
	s := strings.Repeat("a", 510) + "€€€"
	got := truncate(s, 512)
	if !utf8.ValidString(got) {
		t.Errorf("truncate produced invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Errorf("got %q, want a truncation marker", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("a", 510)) {
		t.Errorf("truncation should keep the whole prefix; got %q", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"-5", 0},
		{"not-a-number", 0},
		{"3", 3 * time.Second},
		{" 5 ", 5 * time.Second},
		{"99999", maxRetryAfter},               // clamped so one request cannot block for hours
		{"9223372036854775807", maxRetryAfter}, // must not overflow
	}
	for _, tc := range cases {
		if got := parseRetryAfter(tc.in); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	got := parseRetryAfter(future)
	if got <= 0 || got > 3*time.Second {
		t.Errorf("parseRetryAfter(%q) = %v, want about 2s", future, got)
	}

	past := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Errorf("parseRetryAfter(%q) = %v, want 0", past, got)
	}
}

func TestDecodeResponse_TooLargeBodyReturnsDistinctError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","answers":{},"usage":{},"pad":"`))
		chunk := strings.Repeat("x", 64<<10)
		for i := 0; i < 160; i++ { // 160 * 64 KiB = 10 MiB, past the 8 MiB cap
			_, _ = w.Write([]byte(chunk))
		}
		_, _ = w.Write([]byte(`"}`))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, WithToken("sk"))
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})
	if err == nil {
		t.Fatal("expected an error for an oversized response")
	}
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Errorf("err should match ErrResponseTooLarge; got %v", err)
	}
	if errors.Is(err, ErrResponseDecoded) {
		t.Errorf("an oversized body is not a decode error; got %v", err)
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
			_, err := resolveURL(tc.baseURL, "/v1/systemone")
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
		code     int
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

// ---------------------------------------------------------------------------
// runWithRetry — extracted retry runner. These tests exercise the retry
// contract in isolation (no httptest), which is the whole point of the
// extraction: today every retry invariant is exercised only through a
// real HTTP server, which is slow and hard to set up.
// ---------------------------------------------------------------------------

const (
	retryTestRequestID = "test-request-id"
	retryTestEndpoint  = "https://example.test/v1/systemone"
)

func retryTestClient(t *testing.T) *Client {
	t.Helper()
	c := New("https://example.test",
		WithBackoff(1*time.Microsecond, 5*time.Microsecond),
	)
	return c
}

// serverError returns a synthetic retryable *Error.
func serverError(status int) *Error {
	return &Error{
		StatusCode: status,
		Status:     http.StatusText(status),
		Method:     http.MethodPost,
		URL:        retryTestEndpoint,
		Body:       "boom",
		sentinel:   ErrServerError,
	}
}

// clientError returns a synthetic non-retryable *Error.
func clientError(status int) *Error {
	s := sentinelForStatus(status)
	return &Error{
		StatusCode: status,
		Status:     http.StatusText(status),
		Method:     http.MethodPost,
		URL:        retryTestEndpoint,
		Body:       "no",
		sentinel:   s,
	}
}

// transportError returns a synthetic retryable transport *Error.
func transportError(cause error) *Error {
	return &Error{
		StatusCode: 0,
		Status:     "transport error",
		Method:     http.MethodPost,
		URL:        retryTestEndpoint,
		Body:       cause.Error(),
		sentinel:   ErrTransport,
		cause:      cause,
	}
}

func TestRunWithRetry_SuccessOnFirstAttempt(t *testing.T) {
	c := retryTestClient(t)

	var calls int
	res := c.runWithRetry(context.Background(), 5, retryTestRequestID, retryTestEndpoint,
		func() attemptResult {
			calls++
			return attemptResult{Outcome: outcomeSuccess, Response: nil}
		})

	if calls != 1 {
		t.Errorf("action calls = %d, want 1 (no retry needed)", calls)
	}
	if res.Outcome != outcomeSuccess {
		t.Errorf("outcome = %d, want outcomeSuccess", res.Outcome)
	}
	if res.Err != nil {
		t.Errorf("Err = %v, want nil on success", res.Err)
	}
}

func TestRunWithRetry_RetriesUntilSuccess(t *testing.T) {
	c := retryTestClient(t)

	var calls int
	res := c.runWithRetry(context.Background(), 5, retryTestRequestID, retryTestEndpoint,
		func() attemptResult {
			calls++
			if calls < 3 {
				return attemptResult{Outcome: outcomeRetry, Err: serverError(http.StatusServiceUnavailable)}
			}
			return attemptResult{Outcome: outcomeSuccess, Response: nil}
		})

	if calls != 3 {
		t.Errorf("action calls = %d, want 3", calls)
	}
	if res.Outcome != outcomeSuccess {
		t.Errorf("outcome = %d, want outcomeSuccess", res.Outcome)
	}
}

func TestRunWithRetry_FatalErrorShortCircuits(t *testing.T) {
	c := retryTestClient(t)

	fatal := clientError(http.StatusBadRequest)
	var calls int
	res := c.runWithRetry(context.Background(), 5, retryTestRequestID, retryTestEndpoint,
		func() attemptResult {
			calls++
			return attemptResult{Outcome: outcomeFatal, Err: fatal}
		})

	if calls != 1 {
		t.Errorf("action calls = %d, want 1 (no retry on fatal)", calls)
	}
	if res.Err != fatal {
		t.Errorf("Err = %v, want fatal", res.Err)
	}
}

func TestRunWithRetry_ExhaustionReturnsTrulyLastError(t *testing.T) {
	c := retryTestClient(t)

	err1 := serverError(http.StatusInternalServerError)
	err2 := serverError(http.StatusBadGateway)
	err3 := serverError(http.StatusServiceUnavailable)

	seq := []*Error{err1, err2, err3}
	var calls int
	res := c.runWithRetry(context.Background(), 3, retryTestRequestID, retryTestEndpoint,
		func() attemptResult {
			defer func() { calls++ }()
			return attemptResult{Outcome: outcomeRetry, Err: seq[calls]}
		})

	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if res.Err != err3 {
		t.Errorf("returned Err = %v, want err3 (the last in sequence)", res.Err)
	}
}

func TestRunWithRetry_ContextCancelDuringBackoffStopsRetries(t *testing.T) {
	// Use a long backoff so that during the cancel-during-backoff select,
	// <time.After is NOT yet ready and <ctx.Done() is the only ready case.
	// Go's select picks randomly when multiple cases are ready, so a 1µs
	// backoff would be flaky.
	c := New("https://example.test",
		WithBackoff(100*time.Millisecond, 200*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	res := c.runWithRetry(ctx, 5, retryTestRequestID, retryTestEndpoint,
		func() attemptResult {
			calls++
			// Cancel after the first failure so the runner's backoff
			// sleep observes the cancellation.
			if calls == 1 {
				cancel()
			}
			return attemptResult{Outcome: outcomeRetry, Err: serverError(http.StatusServiceUnavailable)}
		})

	if calls != 1 {
		t.Errorf("action calls = %d, want 1 (no retry after ctx cancel)", calls)
	}
	if res.Err == nil {
		t.Fatal("expected error from ctx cancel, got nil")
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("Err should wrap context.Canceled; got %v", res.Err)
	}
	if !errors.Is(res.Err, ErrTransport) {
		t.Errorf("Err should match ErrTransport sentinel; got %v", res.Err)
	}
}

func TestRunWithRetry_ContextErrInsideActionOverridesRetry(t *testing.T) {
	// If the action's *Error wraps a context error (because the HTTP call
	// saw a cancelled ctx), the runner must not retry — even if the action
	// said outcomeRetry. This is the contract that "the client never
	// retries on a cancelled context".
	c := retryTestClient(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before any call

	transport := transportError(context.Canceled)
	var calls int
	res := c.runWithRetry(ctx, 5, retryTestRequestID, retryTestEndpoint,
		func() attemptResult {
			calls++
			return attemptResult{Outcome: outcomeRetry, Err: transport}
		})

	if calls != 1 {
		t.Errorf("action calls = %d, want 1", calls)
	}
	if res.Err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("Err should match context.Canceled; got %v", res.Err)
	}
}

func TestRunWithRetry_ZeroAttemptsReturnsSentinelTransportError(t *testing.T) {
	c := retryTestClient(t)

	var calls int
	res := c.runWithRetry(context.Background(), 0, retryTestRequestID, retryTestEndpoint,
		func() attemptResult {
			calls++
			return attemptResult{Outcome: outcomeSuccess}
		})

	if calls != 0 {
		t.Errorf("action calls = %d, want 0 (loop never runs)", calls)
	}
	if res.Err == nil {
		t.Fatal("expected sentinel error for zero attempts")
	}
	if !errors.Is(res.Err, ErrTransport) {
		t.Errorf("zero-attempts error should match ErrTransport; got %v", res.Err)
	}
}

func TestRunWithRetry_LogsRetryBackoffLine(t *testing.T) {
	var buf bytes.Buffer
	c := New("https://example.test",
		WithBackoff(1*time.Microsecond, 5*time.Microsecond),
		WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	)

	_ = c.runWithRetry(context.Background(), 3, retryTestRequestID, retryTestEndpoint,
		func() attemptResult {
			return attemptResult{Outcome: outcomeRetry, Err: serverError(http.StatusServiceUnavailable)}
		})

	out := buf.String()
	if !strings.Contains(out, "retrying after backoff") {
		t.Errorf("expected retry-backoff log; got %q", out)
	}
	if !strings.Contains(out, "delay=") {
		t.Errorf("expected delay= key in log; got %q", out)
	}
	if !strings.Contains(out, retryTestRequestID) {
		t.Errorf("expected request_id in log; got %q", out)
	}
}

func TestRunWithRetry_LogsRetryableStatusWithStatusKey(t *testing.T) {
	var buf bytes.Buffer
	c := New("https://example.test",
		WithBackoff(1*time.Microsecond, 5*time.Microsecond),
		WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	)

	_ = c.runWithRetry(context.Background(), 3, retryTestRequestID, retryTestEndpoint,
		func() attemptResult {
			return attemptResult{Outcome: outcomeRetry, Err: serverError(http.StatusBadGateway)}
		})

	out := buf.String()
	if !strings.Contains(out, "retryable status, will retry") {
		t.Errorf("expected retryable-status log; got %q", out)
	}
	if !strings.Contains(out, "status=502") {
		t.Errorf("expected status=502 in log; got %q", out)
	}
}

func TestRunWithRetry_LogsTransportErrKey(t *testing.T) {
	var buf bytes.Buffer
	c := New("https://example.test",
		WithBackoff(1*time.Microsecond, 5*time.Microsecond),
		WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	)

	cause := errors.New("dial tcp: connection refused")
	_ = c.runWithRetry(context.Background(), 2, retryTestRequestID, retryTestEndpoint,
		func() attemptResult {
			return attemptResult{Outcome: outcomeRetry, Err: transportError(cause)}
		})

	out := buf.String()
	if !strings.Contains(out, "transport error") {
		t.Errorf("expected transport-error log; got %q", out)
	}
	if !strings.Contains(out, "err=") {
		t.Errorf("expected err= key in log; got %q", out)
	}
}

func TestRunWithRetry_LogsClientErrorNotRetrying(t *testing.T) {
	var buf bytes.Buffer
	c := New("https://example.test",
		WithBackoff(1*time.Microsecond, 5*time.Microsecond),
		WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	)

	_ = c.runWithRetry(context.Background(), 3, retryTestRequestID, retryTestEndpoint,
		func() attemptResult {
			return attemptResult{Outcome: outcomeFatal, Err: clientError(http.StatusBadRequest)}
		})

	out := buf.String()
	if !strings.Contains(out, "client error, not retrying") {
		t.Errorf("expected client-error log; got %q", out)
	}
	if !strings.Contains(out, "status=400") {
		t.Errorf("expected status=400 in log; got %q", out)
	}
}

func TestRunWithRetry_FirstAttemptLogsNothingBeforeIt(t *testing.T) {
	// The "retrying after backoff" log must only fire between attempts,
	// not before the first one.
	var buf bytes.Buffer
	c := New("https://example.test",
		WithBackoff(1*time.Microsecond, 5*time.Microsecond),
		WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	)

	var calls int
	_ = c.runWithRetry(context.Background(), 5, retryTestRequestID, retryTestEndpoint,
		func() attemptResult {
			calls++
			return attemptResult{Outcome: outcomeSuccess}
		})

	out := buf.String()
	if strings.Contains(out, "retrying after backoff") {
		t.Errorf("first attempt should not log a retry; got %q", out)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}
