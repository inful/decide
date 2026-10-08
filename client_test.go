package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	c := New("https://example.test", WithToken("sk-gw-xyz"))
	if c.token != "sk-gw-xyz" {
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
	if got := c.newRequestID(); got != "fixed-id" {
		t.Errorf("newRequestID = %q, want fixed-id", got)
	}
	if calls != 1 {
		t.Errorf("custom func not invoked; calls = %d", calls)
	}
}

func TestNewRequestID_DefaultsToUUIDShape(t *testing.T) {
	c := New("https://example.test")
	id := c.newRequestID()
	if len(id) != 36 {
		t.Errorf("UUID-shaped length = %d, want 36", len(id))
	}
	if strings.Count(id, "-") != 4 {
		t.Errorf("UUID-shaped has %d dashes, want 4", strings.Count(id, "-"))
	}
}

func TestClient_SystemOne_HappyPath(t *testing.T) {
	var (
		gotMethod     string
		gotAuth       string
		gotContentType string
		gotBody       []byte
		gotReqID      string
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

	c := New(srv.URL, WithToken("sk-gw-test"))
	resp, err := c.SystemOne(context.Background(), Request{
		State: State{Body: "Jeg ble fakturert to ganger, jeg vil ha pengene tilbake."},
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
	if gotAuth != "Bearer sk-gw-test" {
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
		WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		))

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

	c := New(srv.URL + "/", WithToken("sk"))
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