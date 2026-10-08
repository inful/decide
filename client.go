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
	"strings"
	"time"
)

// systemOnePath is the API path used by SystemOne.
const systemOnePath = "/v1/systemone"

// requestIDHeader is the header used for request correlation in both
// directions: outgoing requests set it, incoming responses may echo it.
const requestIDHeader = "X-Request-ID"

// responseBodyCap caps the size of a response body the client will read into
// memory; responses beyond this are not a server contract we promise to honor.
const responseBodyCap = 8 << 20 // 8 MiB

// Client is the entry point for the library. Configure with New and customize
// with functional Options. A Client is safe for concurrent use.
type Client struct {
	baseURL    string
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
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 60 * time.Second},
		retry:      DefaultRetryPolicy(),
		requestID:  newUUIDv4,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.logger = newLoggerOrDiscard(c.logger)
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: 60 * time.Second}
	}
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
// Pass a negative value to keep the default but allow the call site to also
// pass WithBackoff; pass 0 to disable retries entirely.
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
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

func (c *Client) newRequestID() string {
	return c.requestID()
}

func newLoggerOrDiscard(l Logger) Logger {
	if l == nil {
		return DiscardLogger{}
	}
	return l
}

// --- The call ---------------------------------------------------------------

// SystemOne posts a Request to /v1/systemone and returns the parsed Response.
// Non-2xx responses and transport errors are returned as *Error.
func (c *Client) SystemOne(ctx context.Context, req Request) (*Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint, err := c.resolve(systemOnePath)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("decide: marshal request: %w", err)
	}

	requestID := RequestIDFromContext(ctx)
	if requestID == "" {
		requestID = c.newRequestID()
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

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			delay := c.retry.nextDelay(attempt - 1)
			c.logger.Info("systemone: retrying after backoff",
				"attempt", attempt+1,
				"of", attempts,
				"delay", delay,
				"endpoint", endpoint,
				"request_id", requestID,
			)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		httpResp, transportErr := c.do(ctx, http.MethodPost, endpoint, body, requestID)
		if transportErr != nil {
			lastErr = transportErr
			if isContextErr(transportErr) {
				return nil, transportErr
			}
			c.logger.Warn("systemone: transport error",
				"attempt", attempt+1,
				"of", attempts,
				"request_id", requestID,
				"err", transportErr.Error(),
			)
			continue
		}

		if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
			return c.decodeResponse(httpResp)
		}

		respBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
		_ = httpResp.Body.Close()
		httpErr := newHTTPError(http.MethodPost, endpoint, httpResp, respBody)

		if !httpErr.IsRetryable() {
			c.logger.Info("systemone: client error, not retrying",
				"status", httpResp.StatusCode,
				"request_id", requestID,
			)
			return nil, httpErr
		}

		c.logger.Warn("systemone: retryable status, will retry",
			"status", httpResp.StatusCode,
			"attempt", attempt+1,
			"of", attempts,
			"request_id", requestID,
		)
		lastErr = httpErr
	}

	return nil, lastErr
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

func (c *Client) decodeResponse(httpResp *http.Response) (*Response, error) {
	defer func() { _ = httpResp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, responseBodyCap))
	if err != nil {
		return nil, wrapTransportError(http.MethodPost, httpResp.Request.URL.String(), err)
	}

	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &Error{
			StatusCode: httpResp.StatusCode,
			Status:     httpResp.Status,
			Method:     http.MethodPost,
			URL:        httpResp.Request.URL.String(),
			Body:       fmt.Sprintf("decode error: %v; raw: %s", err, truncate(string(body), 512)),
			sentinel:   ErrResponseDecoded,
		}
	}
	resp.requestID = httpResp.Header.Get(requestIDHeader)
	return &resp, nil
}

func (c *Client) resolve(path string) (string, error) {
	if c.baseURL == "" {
		return "", errors.New("decide: base URL is empty")
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("decide: parse base URL %q: %w", c.baseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("decide: base URL %q must include scheme and host", c.baseURL)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}