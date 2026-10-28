package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNew_DefaultOptions(t *testing.T) {
	c := New("https://example.test")
	if c == nil {
		t.Fatal("New returned nil")
	}
	if c.baseURL != "https://example.test" {
		t.Errorf("baseURL = %q", c.baseURL)
	}
	if c.retry.MaxRetries < 1 {
		t.Errorf("default retry = %v, want >= 1 attempt", c.retry)
	}
	if c.httpClient == nil {
		t.Errorf("default httpClient should not be nil")
	}
	if c.requestID == nil {
		t.Errorf("default requestID func should not be nil")
	}
}

func TestWithToken(t *testing.T) {
	c := New("https://example.test", WithToken("<TOKEN>"))
	if c.token != "<TOKEN>" {
		t.Errorf("token = %q", c.token)
	}
}

func TestWithHTTPClient(t *testing.T) {
	custom := &http.Client{Timeout: 7 * time.Second}
	c := New("https://example.test", WithHTTPClient(custom))
	if c.httpClient != custom {
		t.Errorf("httpClient not set to the supplied client")
	}
}

func TestWithRetries_ZeroDisables(t *testing.T) {
	c := New("https://example.test", WithRetries(0))
	if c.retry.MaxRetries != 0 {
		t.Errorf("MaxRetries = %d, want 0 (disabled)", c.retry.MaxRetries)
	}
}

func TestWithRequestIDFunc_Override(t *testing.T) {
	calls := 0
	c := New("https://example.test", WithRequestIDFunc(func() string {
		calls++
		return "fixed-id"
	}))
	if got := c.requestID(); got != "fixed-id" {
		t.Errorf("requestID = %q, want fixed-id", got)
	}
	if calls != 1 {
		t.Errorf("custom func not invoked; calls = %d", calls)
	}
}

func TestNewRequestID_DefaultsToUUIDShape(t *testing.T) {
	c := New("https://example.test")
	id := c.requestID()
	if len(id) != 36 {
		t.Errorf("UUID-shaped length = %d, want 36", len(id))
	}
	if strings.Count(id, "-") != 4 {
		t.Errorf("UUID-shaped has %d dashes, want 4", strings.Count(id, "-"))
	}
}

func TestClient_SystemOne_HappyPath(t *testing.T) {
	var (
		gotMethod      string
		gotAuth        string
		gotContentType string
		gotBody        []byte
		gotReqID       string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotReqID = r.Header.Get("X-Request-ID")
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "server-echo-id")
		_, _ = w.Write([]byte(responseFixture))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, WithToken("<TOKEN>"))
	resp, err := c.SystemOne(context.Background(), Request{
		State: State{Body: "I was charged twice, I want a refund."},
		Questions: Questions{
			"department": NewChoice("Which?", map[string]string{"billing": "invoices"}),
		},
	})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotAuth != "Bearer <TOKEN>" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotReqID == "" {
		t.Errorf("X-Request-ID header not set on outgoing request")
	}

	var sentReq Request
	if err := json.Unmarshal(gotBody, &sentReq); err != nil {
		t.Fatalf("server-side unmarshal of request body: %v\nbody=%s", err, gotBody)
	}
	if sentReq.State.Body == "" {
		t.Errorf("server saw empty state body")
	}
	if _, ok := sentReq.Questions["department"]; !ok {
		t.Errorf("server did not see department question")
	}

	if resp.Model != "laya-rl-agent" {
		t.Errorf("response model = %q", resp.Model)
	}
	if len(resp.Answers) != 2 {
		t.Errorf("response answers length = %d", len(resp.Answers))
	}
	if resp.RequestID() != "server-echo-id" {
		t.Errorf("response RequestID() = %q, want server-echo-id", resp.RequestID())
	}
}

func TestClient_SystemOne_PropagatesRequestIDFromContext(t *testing.T) {
	var gotReqID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReqID = r.Header.Get("X-Request-ID")
		_, _ = w.Write([]byte(`{"model":"x","answers":{},"usage":{}}`))
	}))
	t.Cleanup(srv.Close)

	ctx := WithContextRequestID(context.Background(), "caller-supplied-id")
	c := New(srv.URL, WithToken("sk"))
	_, err := c.SystemOne(ctx, Request{State: State{Body: "hi"}})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if gotReqID != "caller-supplied-id" {
		t.Errorf("server saw X-Request-ID = %q, want caller-supplied-id", gotReqID)
	}
}

func TestClient_SystemOne_4xxReturnsTypedErrorNoRetry(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, `{"error":"bad state"}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, WithToken("sk"), WithRetries(5))
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})

	if err == nil {
		t.Fatal("expected error for 400")
	}
	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("err should match ErrBadRequest sentinel; got %v", err)
	}
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no retry on 4xx)", attempts.Load())
	}
}

func TestClient_SystemOne_429Then200RetriesThenSucceeds(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(responseFixture))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, WithToken("sk"), WithRetries(5))
	resp, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3", attempts.Load())
	}
	if len(resp.Answers) == 0 {
		t.Errorf("expected answers, got %#v", resp)
	}
}

func TestClient_SystemOne_5xxExhaustsRetriesAndFails(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, WithToken("sk"), WithRetries(2))
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})

	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if !errors.Is(err, ErrServerError) {
		t.Errorf("err should match ErrServerError; got %v", err)
	}
	if got, want := attempts.Load(), int32(3); got != want {
		t.Errorf("attempts = %d, want %d (1 + 2 retries)", got, want)
	}
}

func TestClient_SystemOne_ContextCancellationStopsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, WithToken("sk"), WithRetries(5), WithBackoff(10*time.Millisecond, 50*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the first attempt sees a cancelled context

	_, err := c.SystemOne(ctx, Request{State: State{Body: "hi"}})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled; got %v", err)
	}
}

func TestClient_SystemOne_LoggerObservesRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "transient", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	c := New(srv.URL,
		WithToken("sk"),
		WithRetries(2),
		WithBackoff(1*time.Millisecond, 5*time.Millisecond),
		WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))))

	_, _ = c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})

	out := buf.String()
	if !strings.Contains(out, "systemone") {
		t.Errorf("expected log output to mention systemone; got %q", out)
	}
	if !strings.Contains(out, "retry") {
		t.Errorf("expected retry log line; got %q", out)
	}
}

func TestClient_SystemOne_NilLoggerIsAllowed(t *testing.T) {
	// WithLogger(nil) means "use the discard logger"; the call must not panic.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(responseFixture))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, WithToken("sk"), WithLogger(nil))
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
}

func TestClient_AppendsV1SystemOnePath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(responseFixture))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL+"/", WithToken("sk"))
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if gotPath != "/v1/systemone" {
		t.Errorf("path = %q, want /v1/systemone", gotPath)
	}
}

func TestClient_BaseURLValidation(t *testing.T) {
	if c := New(""); c.baseURL != "" {
		t.Errorf("empty baseURL should be preserved (caller decides whether to use it)")
	}
	if c := New("not-a-url"); c.baseURL != "not-a-url" {
		t.Errorf("garbage baseURL should be preserved; validation happens at call time")
	}
}

func TestClient_ResolvesEndpointOnceAtConstruction(t *testing.T) {
	c := New("https://example.test/")
	if c.endpoint != "https://example.test/v1/systemone" {
		t.Errorf("endpoint = %q, want %q", c.endpoint, "https://example.test/v1/systemone")
	}
	if c.resolveErr != nil {
		t.Errorf("resolveErr = %v, want nil", c.resolveErr)
	}
}

func TestClient_InvalidBaseURLIsReportedAtCallTime(t *testing.T) {
	c := New("not-a-url")
	if c.resolveErr == nil {
		t.Fatal("New should have recorded the resolve error")
	}
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})
	if err == nil {
		t.Fatal("expected SystemOne to report the bad base URL")
	}
}

func TestClient_SystemOne_InvalidRequestIDFailsFast(t *testing.T) {
	// A request ID with a newline cannot be sent as a header. The client
	// must say so instead of failing every attempt as a transport error.
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		_, _ = w.Write([]byte(responseFixture))
	}))
	t.Cleanup(srv.Close)

	ctx := WithContextRequestID(context.Background(), "bad\nvalue")
	c := New(srv.URL, WithToken("sk"))
	_, err := c.SystemOne(ctx, Request{State: State{Body: "hi"}})

	if err == nil {
		t.Fatal("expected an error for an invalid request ID")
	}
	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("err should match ErrBadRequest; got %v", err)
	}
	if attempts.Load() != 0 {
		t.Errorf("server saw %d attempts, want 0 (the request must not be sent)", attempts.Load())
	}
	if strings.Contains(err.Error(), "transport") {
		t.Errorf("error should name the real cause, not a transport failure; got %v", err)
	}
	e, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if e.IsRetryable() {
		t.Error("an invalid request ID is not retryable")
	}
}

func TestClient_SystemOne_LargeErrorBodyIsTrimmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 512<<10))) // 512 KiB
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, WithToken("sk"), WithRetries(0))
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})
	e, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if len(e.Body) > errorBodyKeep+len("...(truncated)") {
		t.Errorf("Error.Body is %d bytes, want at most %d", len(e.Body), errorBodyKeep)
	}
}

func TestRequestIDFromContext_NilContext(t *testing.T) {
	//nolint:staticcheck // deliberately exercising the nil-context guard
	if got := RequestIDFromContext(nil); got != "" {
		t.Errorf("RequestIDFromContext(nil) = %q, want %q", got, "")
	}
}

func TestWithRetries_NegativeKeepsTheDefault(t *testing.T) {
	c := New("https://example.test", WithRetries(-1))
	if c.retry.MaxRetries != DefaultRetryPolicy().MaxRetries {
		t.Errorf("MaxRetries = %d, want the default %d", c.retry.MaxRetries, DefaultRetryPolicy().MaxRetries)
	}
}

func TestClient_SystemOne_RequestIDFallsBackToSentID(t *testing.T) {
	// The server does not echo X-Request-ID. The response must still carry
	// the ID the client sent, so the caller can always correlate.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(responseFixture))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, WithToken("sk"), WithRequestIDFunc(func() string { return "sent-id" }))
	resp, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if got := resp.RequestID(); got != "sent-id" {
		t.Errorf("RequestID() = %q, want %q (the ID the client sent)", got, "sent-id")
	}
}

func TestClient_SystemOne_RequestIDFallsBackToContextID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(responseFixture))
	}))
	t.Cleanup(srv.Close)

	ctx := WithContextRequestID(context.Background(), "upstream-id")
	c := New(srv.URL, WithToken("sk"))
	resp, err := c.SystemOne(ctx, Request{State: State{Body: "hi"}})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if got := resp.RequestID(); got != "upstream-id" {
		t.Errorf("RequestID() = %q, want %q", got, "upstream-id")
	}
}

func TestClient_SystemOne_HonorsRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(responseFixture))
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	c := New(srv.URL, WithToken("sk"), WithRetries(3),
		WithBackoff(1*time.Millisecond, 5*time.Millisecond),
		WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	)

	start := time.Now()
	resp, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if len(resp.Answers) == 0 {
		t.Fatalf("expected answers, got %#v", resp)
	}
	// The server asked for a 1s wait; the computed backoff was 1ms.
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("retried after %v, want at least ~1s (Retry-After must be honored)", elapsed)
	}
	if !strings.Contains(buf.String(), "delay=1s") {
		t.Errorf("expected the logged delay to be 1s from Retry-After; got %q", buf.String())
	}
}

func TestNew_TypedNilLoggerIsIgnored(t *testing.T) {
	// A nil *slog.Logger stored in a Logger interface is not a nil
	// interface; calling it would panic. The client must fall back to the
	// discard logger instead.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(responseFixture))
	}))
	t.Cleanup(srv.Close)

	var lg *slog.Logger
	c := New(srv.URL, WithToken("sk"), WithLogger(lg))
	if _, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}}); err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
}

func TestNewTransport_RaisesIdleConnectionLimit(t *testing.T) {
	tr := newTransport(http.DefaultTransport)
	if tr.MaxIdleConnsPerHost < 100 {
		t.Errorf("MaxIdleConnsPerHost = %d, want >= 100", tr.MaxIdleConnsPerHost)
	}
	if tr == http.DefaultTransport.(*http.Transport) {
		t.Error("newTransport must copy, not mutate, the base transport")
	}
}

func TestNewTransport_CustomRoundTripperDoesNotPanic(t *testing.T) {
	// Someone replaced http.DefaultTransport with a non-*Transport. The
	// package must still initialise.
	tr := newTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, nil
	}))
	if tr == nil {
		t.Fatal("newTransport returned nil")
	}
	if tr.MaxIdleConnsPerHost < 100 {
		t.Errorf("MaxIdleConnsPerHost = %d, want >= 100", tr.MaxIdleConnsPerHost)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDefaultTransportReusesConnections(t *testing.T) {
	// Go's default transport keeps only 2 idle connections per host. This
	// client talks to one host, so bursts would otherwise reopen a
	// connection (and a TLS handshake) every time.
	if defaultTransport.MaxIdleConnsPerHost < 100 {
		t.Fatalf("default transport MaxIdleConnsPerHost = %d, want >= 100",
			defaultTransport.MaxIdleConnsPerHost)
	}

	var opened atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(responseFixture))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	c := New(srv.URL, WithToken("sk"))

	burst := func() {
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})
			}()
		}
		wg.Wait()
	}

	burst()
	first := opened.Load()
	burst()
	second := opened.Load() - first

	if second > 2 {
		t.Errorf("second burst of 10 concurrent calls opened %d new connections, want ~0 (pool reuse broken)", second)
	}
}
