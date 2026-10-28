package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

// systemOnePath is the API path used by SystemOne.
const systemOnePath = "/v1/systemone"

// requestIDHeader is the header used for request correlation in both
// directions: outgoing requests set it, incoming responses may echo it.
const requestIDHeader = "X-Request-ID"

// responseBodyCap caps the size of a response body the client will read into
// memory; responses beyond this are not a server contract we promise to honor.
const responseBodyCap = 8 << 20 // 8 MiB

// errorBodyCap caps how much of a non-2xx response body is read from the
// wire. Reading the whole body is what lets the connection be reused.
const errorBodyCap = 1 << 20 // 1 MiB

// errorBodyKeep caps how much of that body is kept in Error.Body. The body
// is a diagnostic aid, so a few KB is plenty and it keeps a failed call from
// holding a megabyte.
const errorBodyKeep = 4 << 10 // 4 KiB

// maxRetryAfter caps how long the client will honor a server's Retry-After
// header. Longer waits fall back to the normal backoff, so one request can
// never pin a goroutine for minutes on a hostile or buggy server.
const maxRetryAfter = 30 * time.Second

// defaultTransport is the transport shared by every Client created without
// WithHTTPClient.
//
// Go's http.DefaultTransport keeps only 2 idle connections per host
// (DefaultMaxIdleConnsPerHost). This client talks to exactly one API host,
// so any burst past two in-flight requests opens a fresh TCP connection —
// and a fresh TLS handshake over HTTPS. Cloning the default and raising the
// per-host limit keeps connections alive between bursts.
var defaultTransport = newTransport(http.DefaultTransport)

// newTransport copies base and raises its per-host idle connection limit.
func newTransport(base http.RoundTripper) *http.Transport {
	tr, ok := base.(*http.Transport)
	if !ok {
		// The default transport was replaced with a custom RoundTripper.
		// Fall back to a plain transport rather than panicking during
		// package init.
		tr = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	out := tr.Clone()
	out.MaxIdleConnsPerHost = 100
	return out
}

// Client is the entry point for the library. Configure with New and customize
// with functional Options. A Client is safe for concurrent use.
type Client struct {
	baseURL    string
	endpoint   string
	resolveErr error
	token      string
	httpClient *http.Client
	retry      RetryPolicy
	logger     Logger
	requestID  func() string
}

// Option mutates a Client during construction.
type Option func(*Client)

// New returns a Client pointing at baseURL. The baseURL should be the scheme +
// host (e.g. "https://api.example.com"); the SystemOne path is appended
// automatically.
//
// A baseURL that cannot be turned into an endpoint is not rejected here — New
// does not return an error. The problem is reported by the first call.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Transport: defaultTransport, Timeout: 60 * time.Second},
		retry:      DefaultRetryPolicy(),
		requestID:  newUUIDv4,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.logger = newLoggerOrDiscard(c.logger)
	// The endpoint never changes, so build it once instead of on every call.
	c.endpoint, c.resolveErr = resolveURL(c.baseURL, systemOnePath)
	return c
}

// WithToken sets the bearer token used in the Authorization header.
func WithToken(token string) Option {
	return func(c *Client) { c.token = token }
}

// WithHTTPClient swaps the default *http.Client. Useful for setting custom
// transports, proxies, or timeouts. Nil values are ignored.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithRetries sets the maximum number of retries after the initial attempt.
// Pass a negative value to leave the default unchanged; pass 0 to disable
// retries entirely.
func WithRetries(n int) Option {
	return func(c *Client) {
		if n < 0 {
			return
		}
		c.retry.MaxRetries = n
	}
}

// WithBackoff overrides the retry backoff schedule. Useful in tests to keep
// retry loops fast.
func WithBackoff(initial, max time.Duration) Option {
	return func(c *Client) {
		if initial > 0 {
			c.retry.InitialBackoff = initial
		}
		if max > 0 {
			c.retry.MaxBackoff = max
		}
	}
}

// WithLogger installs a Logger. A nil logger falls back to a discard logger.
func WithLogger(l Logger) Option {
	return func(c *Client) { c.logger = l }
}

// WithRequestIDFunc overrides the function used to mint X-Request-IDs when
// the caller hasn't supplied one via the context. Nil is ignored.
func WithRequestIDFunc(f func() string) Option {
	return func(c *Client) {
		if f != nil {
			c.requestID = f
		}
	}
}

// --- Context plumbing for request IDs ---------------------------------------

type ctxKey struct{ name string }

var requestIDKey = ctxKey{"decide.request-id"}

// WithContextRequestID returns a child context that carries the given request
// ID. If set, the client uses it instead of minting a fresh one.
func WithContextRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the request ID stored in ctx, or "" if none.
// A nil context yields "".
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// validHeaderValue reports whether v is safe to use as an HTTP header value.
// Go's transport rejects control characters, so we catch them here instead of
// letting every attempt fail with a transport error.
func validHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		b := v[i]
		if b == '\t' {
			continue
		}
		if b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

// newLoggerOrDiscard returns l, or a logger that drops everything if l is
// unusable.
//
// A nil interface and an interface holding a nil pointer are different
// things: the second passes an `l == nil` check but panics the moment it is
// called. Callers often have a `*slog.Logger` field that may be nil, so
// check the pointer too.
func newLoggerOrDiscard(l Logger) Logger {
	if l == nil {
		return DiscardLogger{}
	}
	v := reflect.ValueOf(l)
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return DiscardLogger{}
	}
	return l
}

// --- The call ---------------------------------------------------------------

// attemptOutcome is what the per-attempt action reports back to runWithRetry.
type attemptOutcome int

const (
	// outcomeSuccess: the attempt produced a 2xx response; Response is populated.
	outcomeSuccess attemptOutcome = iota
	// outcomeFatal: the attempt produced a non-retryable failure; Err is
	// populated and runWithRetry returns immediately.
	outcomeFatal
	// outcomeRetry: the attempt produced a retryable failure; Err is
	// populated and runWithRetry sleeps then tries again. A *Error whose
	// cause is a context error is promoted to outcomeFatal regardless of
	// this classification — the client never retries on a cancelled context.
	outcomeRetry
)

// attemptResult is what runWithRetry's action closure returns.
//
// Exactly one of Response or Err is meaningful:
//
//   - Outcome == outcomeSuccess → Response is the *http.Response the caller
//     must consume (typically via decodeResponse).
//   - Outcome != outcomeSuccess → Err is the *Error to surface.
type attemptResult struct {
	Outcome  attemptOutcome
	Response *http.Response
	Err      *Error

	// RetryAfter, when non-zero, replaces the computed backoff before the
	// next attempt. Servers send it on 429 and 503 to say when to come back.
	RetryAfter time.Duration
}

// runWithRetry invokes action up to maxAttempts times, sleeping
// c.retry.nextDelay between attempts. It returns the first successful
// attemptResult or, after exhaustion, the last retryable error.
//
// Context cancellation terminates immediately in two places:
//
//  1. While sleeping between attempts. The returned *Error has ErrTransport
//     as its sentinel and cause set to ctx.Err().
//  2. If action returns an *Error that wraps context.Canceled or
//     context.DeadlineExceeded (via errors.Is), even if the action
//     classified it as outcomeRetry. This preserves the contract that
//     the client never retries on a cancelled context.
//
// Logging keys are stable and identical to the pre-extraction inline loop:
//
//	"systemone: retrying after backoff" (Info):  attempt, of, delay, endpoint, request_id
//	"systemone: transport error"        (Warn):  attempt, of, request_id, err
//	"systemone: client error, not retrying"     (Info): status, request_id
//	"systemone: retryable status, will retry"   (Warn): status, attempt, of, request_id
//
// When the server sent a Retry-After header, the delay before the next
// attempt is that value instead of the computed backoff.
//
// runWithRetry is the seam that lets the retry contract be unit-tested
// without spinning up an HTTP server.
func (c *Client) runWithRetry(
	ctx context.Context,
	maxAttempts int,
	requestID, endpoint string,
	action func() attemptResult,
) attemptResult {
	var last attemptResult
	var wait time.Duration
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := wait
			if delay <= 0 {
				delay = c.retry.nextDelay(attempt - 1)
			}
			c.logger.Info("systemone: retrying after backoff",
				"attempt", attempt+1,
				"of", maxAttempts,
				"delay", delay,
				"endpoint", endpoint,
				"request_id", requestID,
			)
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return attemptResult{
					Outcome: outcomeFatal,
					Err:     wrapTransportError(http.MethodPost, endpoint, ctx.Err()),
				}
			}
		}

		res := action()
		wait = res.RetryAfter
		switch res.Outcome {
		case outcomeSuccess:
			return res

		case outcomeFatal:
			if res.Err != nil && res.Err.StatusCode != 0 {
				c.logger.Info("systemone: client error, not retrying",
					"status", res.Err.StatusCode,
					"request_id", requestID,
				)
			}
			return res

		case outcomeRetry:
			if res.Err != nil && isContextErr(res.Err) {
				return res
			}
			if res.Err != nil && res.Err.StatusCode == 0 {
				c.logger.Warn("systemone: transport error",
					"attempt", attempt+1,
					"of", maxAttempts,
					"request_id", requestID,
					"err", res.Err.Error(),
				)
			} else {
				c.logger.Warn("systemone: retryable status, will retry",
					"status", res.Err.StatusCode,
					"attempt", attempt+1,
					"of", maxAttempts,
					"request_id", requestID,
				)
			}
			last = res
		}
	}
	if last.Err == nil {
		// Defensive: callers should ensure maxAttempts >= 1, but if not,
		// surface a sentinel error rather than panicking.
		return attemptResult{
			Outcome: outcomeFatal,
			Err: &Error{
				StatusCode: 0,
				Status:     "no attempts made",
				Method:     http.MethodPost,
				URL:        endpoint,
				Body:       "decide: retry policy yielded zero attempts",
				sentinel:   ErrTransport,
			},
		}
	}
	return last
}

// SystemOne posts a Request to /v1/systemone and returns the parsed Response.
// Non-2xx responses and transport errors are returned as *Error.
func (c *Client) SystemOne(ctx context.Context, req Request) (*Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.resolveErr != nil {
		return nil, c.resolveErr
	}
	endpoint := c.endpoint

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("decide: marshal request: %w", err)
	}

	requestID := RequestIDFromContext(ctx)
	if requestID == "" {
		requestID = c.requestID()
	}
	// A request ID with a control character cannot be sent as a header.
	// Catch it here, with a message that names the cause, instead of
	// letting the HTTP transport fail every attempt.
	if !validHeaderValue(requestID) {
		return nil, &Error{
			StatusCode: 0,
			Status:     "invalid request ID",
			Method:     http.MethodPost,
			URL:        endpoint,
			Body:       "the request ID contains a character that cannot be sent in an HTTP header",
			sentinel:   ErrBadRequest,
		}
	}

	c.logger.Debug("systemone: sending request",
		"endpoint", endpoint,
		"request_id", requestID,
		"questions", len(req.Questions),
	)

	attempts := c.retry.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}

	result := c.runWithRetry(ctx, attempts, requestID, endpoint,
		func() attemptResult {
			httpResp, transportErr := c.do(ctx, http.MethodPost, endpoint, body, requestID)
			if transportErr != nil {
				return attemptResult{Outcome: outcomeRetry, Err: transportErr}
			}
			if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
				return attemptResult{Outcome: outcomeSuccess, Response: httpResp}
			}
			respBody, readErr := io.ReadAll(io.LimitReader(httpResp.Body, errorBodyCap))
			_ = httpResp.Body.Close()
			httpErr := newHTTPError(http.MethodPost, endpoint, httpResp, respBody)
			if readErr != nil {
				httpErr.Body = fmt.Sprintf("%s (body read failed: %v)", httpErr.Body, readErr)
			}
			if httpErr.IsRetryable() {
				return attemptResult{
					Outcome:    outcomeRetry,
					Err:        httpErr,
					RetryAfter: parseRetryAfter(httpResp.Header.Get("Retry-After")),
				}
			}
			return attemptResult{Outcome: outcomeFatal, Err: httpErr}
		})

	if result.Err != nil {
		return nil, result.Err
	}
	return c.decodeResponse(result.Response, requestID)
}

func (c *Client) do(ctx context.Context, method, url string, body []byte, requestID string) (*http.Response, *Error) {
	httpReq, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, wrapTransportError(method, url, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	httpReq.Header.Set(requestIDHeader, requestID)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, wrapTransportError(method, url, err)
	}
	return httpResp, nil
}

func (c *Client) decodeResponse(httpResp *http.Response, sentRequestID string) (*Response, error) {
	defer func() { _ = httpResp.Body.Close() }()

	// Read one byte past the cap so an oversized body is detected rather
	// than silently cut into an unparseable prefix.
	body, err := io.ReadAll(io.LimitReader(httpResp.Body, responseBodyCap+1))
	if err != nil {
		return nil, wrapTransportError(http.MethodPost, httpResp.Request.URL.String(), err)
	}
	if len(body) > responseBodyCap {
		return nil, &Error{
			StatusCode: httpResp.StatusCode,
			Status:     httpResp.Status,
			Method:     http.MethodPost,
			URL:        httpResp.Request.URL.String(),
			Body:       fmt.Sprintf("response body exceeds the %d byte limit", responseBodyCap),
			sentinel:   ErrResponseTooLarge,
		}
	}

	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		// Only the first 512 bytes of the raw body are kept, plus one more
		// so truncate can tell whether it landed inside a multi-byte
		// character.
		raw := body
		if len(raw) > 513 {
			raw = raw[:513]
		}
		return nil, &Error{
			StatusCode: httpResp.StatusCode,
			Status:     httpResp.Status,
			Method:     http.MethodPost,
			URL:        httpResp.Request.URL.String(),
			Body:       fmt.Sprintf("decode error: %v; raw: %s", err, truncate(string(raw), 512)),
			sentinel:   ErrResponseDecoded,
		}
	}
	resp.requestID = httpResp.Header.Get(requestIDHeader)
	if resp.requestID == "" {
		resp.requestID = sentRequestID
	}
	return &resp, nil
}

// resolveURL turns a base URL and a path into the full endpoint. The base
// URL should be the scheme + host; the path is appended.
func resolveURL(baseURL, path string) (string, error) {
	if baseURL == "" {
		return "", errors.New("decide: base URL is empty")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("decide: parse base URL %q: %w", baseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("decide: base URL %q must include scheme and host", baseURL)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String(), nil
}

// truncate shortens s to at most n bytes and marks that it cut.
//
// The cut never lands inside a multi-byte character: s[:n] would split one
// in half and leave invalid UTF-8 in error messages and log lines.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "...(truncated)"
}
