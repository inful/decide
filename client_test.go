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
	// RFC 4122: the version nibble is 4 and the variant nibble is one of
	// 8, 9, a, b. Without these the ID only looks like a UUID.
	if id[14] != '4' {
		t.Errorf("version nibble = %q, want '4' (id %q)", id[14], id)
	}
	if v := id[19]; !strings.ContainsRune("89ab", rune(v)) {
		t.Errorf("variant nibble = %q, want one of 89ab (id %q)", v, id)
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

	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotReqID = r.Header.Get("X-Request-ID")
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "server-echo-id")
		_, _ = w.Write([]byte(responseFixture))
	})

	c := New(base, WithToken("<TOKEN>"))
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
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotReqID = r.Header.Get("X-Request-ID")
		_, _ = w.Write([]byte(`{"model":"x","answers":{},"usage":{}}`))
	})

	ctx := WithContextRequestID(context.Background(), "caller-supplied-id")
	c := New(base, WithToken("sk"))
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
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, `{"error":"bad state"}`, http.StatusBadRequest)
	})

	c := New(base, WithToken("sk"), WithRetries(5))
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

func TestClient_SystemOne_413IsABadRequest(t *testing.T) {
	// A body over the server's limit comes back as 413. It has to carry a
	// sentinel, or errors.Is matches nothing and the caller cannot tell
	// "shrink your request" from a server fault.
	var attempts atomic.Int32
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, `{"error":"text and schema must not exceed 64 KiB"}`, http.StatusRequestEntityTooLarge)
	})

	c := New(base, WithToken("sk"), WithRetries(5))
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})

	if err == nil {
		t.Fatal("expected error for 413")
	}
	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("413 should match ErrBadRequest; got %v", err)
	}
	if errors.Is(err, ErrServerError) {
		t.Errorf("413 should not match ErrServerError; got %v", err)
	}
	var httpErr *Error
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected a *Error, got %T", err)
	}
	if httpErr.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("StatusCode = %d, want 413", httpErr.StatusCode)
	}
	if httpErr.IsRetryable() {
		t.Error("413 should not be retryable")
	}
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d, want exactly 1 (a smaller request is the fix)", attempts.Load())
	}
}

func TestWithLongContext_Header(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
		want string
	}{
		{"enabled", []Option{WithLongContext()}, "true"},
		{"not enabled", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get(longContextHeader)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(responseFixture))
			})

			client := New(base, append([]Option{WithToken("sk")}, c.opts...)...)
			if _, err := client.SystemOne(context.Background(), Request{State: State{Body: "hi"}}); err != nil {
				t.Fatalf("SystemOne: %v", err)
			}
			if got != c.want {
				t.Errorf("%s = %q, want %q", longContextHeader, got, c.want)
			}
		})
	}
}

func TestClient_SystemOne_RefusesACrossHostRedirect(t *testing.T) {
	// The request body carries the caller's state. A 307 makes Go re-send
	// it to the redirect target, so the client must not follow one that
	// leaves the host the caller chose.
	var gotBody string
	other := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseFixture))
	})

	api := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other+"/v1/systemone", http.StatusTemporaryRedirect)
	})

	c := New(api, WithToken("sk"))
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "secret customer text"}})

	if err == nil {
		t.Fatal("expected an error for a redirect to another host")
	}
	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("a refused redirect should match ErrBadRequest; got %v", err)
	}
	if gotBody != "" {
		t.Errorf("the other host received the request body %q", gotBody)
	}
	var httpErr *Error
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected a *Error, got %T", err)
	}
	if httpErr.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("StatusCode = %d, want 307", httpErr.StatusCode)
	}
}

func TestClient_SystemOne_FollowsASameHostRedirect(t *testing.T) {
	// A redirect that stays on the same host and port is fine.
	var hits atomic.Int32
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/systemone" {
			http.Redirect(w, r, "/v2/systemone", http.StatusTemporaryRedirect)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseFixture))
	})

	c := New(base, WithToken("sk"))
	if _, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}}); err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("the redirect target was hit %d times, want 1", hits.Load())
	}
}

func TestClient_SystemOne_ErrorCarriesTheRequestID(t *testing.T) {
	// The ID is how a caller correlates a failure with the server's log,
	// so it has to be on the error and not only in the client's log line.
	var sent string
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		sent = r.Header.Get("X-Request-ID")
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	})

	c := New(base, WithToken("sk"), WithRetries(0))
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})

	if err == nil {
		t.Fatal("expected an error for a 500")
	}
	var httpErr *Error
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected a *Error, got %T", err)
	}
	if httpErr.RequestID == "" {
		t.Fatal("the error carries no request ID")
	}
	if httpErr.RequestID != sent {
		t.Errorf("error RequestID = %q, want the ID that was sent %q", httpErr.RequestID, sent)
	}
}

func TestClient_SystemOne_ErrorCarriesRetryAfter(t *testing.T) {
	// A caller that turned retries off still needs to know how long the
	// server asked it to wait. It is the server's own number, not the
	// client's 30-second cap on how long it waits.
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"17", 17 * time.Second},
		{"120", 120 * time.Second},
	} {
		t.Run(tc.header+"s", func(t *testing.T) {
			base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", tc.header)
				http.Error(w, `{"error":"slow down"}`, http.StatusTooManyRequests)
			})

			c := New(base, WithToken("sk"), WithRetries(0))
			_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})

			if err == nil {
				t.Fatal("expected an error for a 429")
			}
			var httpErr *Error
			if !errors.As(err, &httpErr) {
				t.Fatalf("expected a *Error, got %T", err)
			}
			if httpErr.RetryAfter != tc.want {
				t.Errorf("RetryAfter = %v, want %v", httpErr.RetryAfter, tc.want)
			}
		})
	}
}

func TestClient_SystemOne_529IsRetried(t *testing.T) {
	// 529 is the API's own overload status, as transient as a 503.
	var attempts atomic.Int32
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(w, `{"error":"overloaded"}`, 529)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseFixture))
	})

	c := New(base, WithToken("sk"), WithRetries(3), WithBackoff(time.Millisecond, time.Millisecond))
	if _, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}}); err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3 (two 529s then success)", got)
	}
}

func TestClient_InvalidBaseURLReturnsTypedError(t *testing.T) {
	// The README says SystemOne returns a *Response or an *Error. A bad
	// base URL used to return a plain error, so errors.As found nothing.
	c := New("not-a-url")
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})

	if err == nil {
		t.Fatal("expected SystemOne to report the bad base URL")
	}
	var httpErr *Error
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected a *Error, got %T", err)
	}
	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("a bad base URL should match ErrBadRequest; got %v", err)
	}
	if httpErr.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 (no response was received)", httpErr.StatusCode)
	}
}

func TestClient_SystemOne_CancelledWaitCarriesTheRequestID(t *testing.T) {
	// The wait between attempts can be cancelled, and a request has
	// already gone out: the caller still needs the ID to correlate it.
	var sent string
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		sent = r.Header.Get("X-Request-ID")
		w.Header().Set("Retry-After", "2")
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	c := New(base, WithToken("sk"), WithRetries(3))
	_, err := c.SystemOne(ctx, Request{State: State{Body: "hi"}})

	if err == nil {
		t.Fatal("expected an error when the context is cancelled")
	}
	var httpErr *Error
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected a *Error, got %T", err)
	}
	if httpErr.RequestID == "" {
		t.Fatal("the cancelled error carries no request ID")
	}
	if httpErr.RequestID != sent {
		t.Errorf("error RequestID = %q, want the ID that was sent %q", httpErr.RequestID, sent)
	}
}

func TestClient_SystemOne_429Then200RetriesThenSucceeds(t *testing.T) {
	var attempts atomic.Int32
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(responseFixture))
	})

	c := New(base, WithToken("sk"), WithRetries(5))
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
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	c := New(base, WithToken("sk"), WithRetries(2))
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
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	c := New(base, WithToken("sk"), WithRetries(5), WithBackoff(10*time.Millisecond, 50*time.Millisecond))

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
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "transient", http.StatusServiceUnavailable)
	})

	var buf bytes.Buffer
	c := New(base,
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
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(responseFixture))
	})

	c := New(base, WithToken("sk"), WithLogger(nil))
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
}

func TestClient_AppendsV1SystemOnePath(t *testing.T) {
	var gotPath string
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(responseFixture))
	})

	c := New(base+"/", WithToken("sk"))
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if gotPath != "/v1/systemone" {
		t.Errorf("path = %q, want /v1/systemone", gotPath)
	}
}

func TestClient_SystemOne_InvalidTokenFailsFast(t *testing.T) {
	// A token with a newline cannot be sent as a header. The client must
	// say so instead of failing every attempt as a transport error.
	// A trailing newline is the classic source: os.Getenv does not strip it.
	var attempts atomic.Int32
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		_, _ = w.Write([]byte(responseFixture))
	})

	c := New(base, WithToken("sk-live-abc\n"))
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})

	if err == nil {
		t.Fatal("expected an error for an invalid token")
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
		t.Error("an invalid token is not retryable")
	}
}

func TestClient_SystemOne_NilContextIsSupported(t *testing.T) {
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(responseFixture))
	})

	c := New(base, WithToken("sk"))
	//nolint:staticcheck // deliberately exercising the nil-context support
	resp, err := c.SystemOne(nil, Request{State: State{Body: "hi"}})
	if err != nil {
		t.Fatalf("SystemOne with a nil context: %v", err)
	}
	if len(resp.Answers) == 0 {
		t.Fatalf("expected answers, got %#v", resp)
	}
}

func TestClient_SystemOne_ErrorBodyReadFailureIsReported(t *testing.T) {
	// If the connection drops part way through the error body, the caller
	// must be able to tell that the body is incomplete.
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("partial"))
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	})

	c := New(base, WithToken("sk"), WithRetries(0))
	_, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	e, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if !strings.Contains(e.Body, "body read failed") {
		t.Errorf("Error.Body should mention the read failure; got %q", e.Body)
	}
}

func TestWithModel_SendsModelInTheBody(t *testing.T) {
	// Ollama requires a "model" field; the hosted APIs let the server route
	// and take no model at all.
	var got map[string]any
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(responseFixture))
	})

	c := New(base, WithToken("sk"), WithModel("laya"))
	if _, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}}); err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if got["model"] != "laya" {
		t.Errorf("body model = %v, want %q", got["model"], "laya")
	}
}

func TestWithModel_NotSentWhenUnset(t *testing.T) {
	var got map[string]any
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(responseFixture))
	})

	c := New(base, WithToken("sk"))
	if _, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}}); err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if _, present := got["model"]; present {
		t.Errorf("body should not carry a model when none is set; got %v", got["model"])
	}
}

func TestRequest_ModelOverridesTheClientDefault(t *testing.T) {
	var got map[string]any
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(responseFixture))
	})

	c := New(base, WithToken("sk"), WithModel("laya"))
	_, err := c.SystemOne(context.Background(), Request{
		Model:     "other-checkpoint",
		State:     State{Body: "hi"},
		Questions: Questions{"q": NewNoul("?")},
	})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if got["model"] != "other-checkpoint" {
		t.Errorf("body model = %v, want %q", got["model"], "other-checkpoint")
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
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		_, _ = w.Write([]byte(responseFixture))
	})

	ctx := WithContextRequestID(context.Background(), "bad\nvalue")
	c := New(base, WithToken("sk"))
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
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 512<<10))) // 512 KiB
	})

	c := New(base, WithToken("sk"), WithRetries(0))
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
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(responseFixture))
	})

	c := New(base, WithToken("sk"), WithRequestIDFunc(func() string { return "sent-id" }))
	resp, err := c.SystemOne(context.Background(), Request{State: State{Body: "hi"}})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if got := resp.RequestID(); got != "sent-id" {
		t.Errorf("RequestID() = %q, want %q (the ID the client sent)", got, "sent-id")
	}
}

func TestClient_SystemOne_RequestIDFallsBackToContextID(t *testing.T) {
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(responseFixture))
	})

	ctx := WithContextRequestID(context.Background(), "upstream-id")
	c := New(base, WithToken("sk"))
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
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(responseFixture))
	})

	var buf bytes.Buffer
	c := New(base, WithToken("sk"), WithRetries(3),
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
	base := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(responseFixture))
	})

	var lg *slog.Logger
	c := New(base, WithToken("sk"), WithLogger(lg))
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
	pool := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(responseFixture))
	}))
	pool.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	pool.Start()
	t.Cleanup(pool.Close)

	c := New(pool.URL, WithToken("sk"))

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
