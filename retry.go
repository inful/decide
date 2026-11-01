package decide

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryPolicy controls how a Client retries failed requests.
//
// MaxRetries is the number of additional attempts after the first (so a value
// of 3 means up to 4 total calls). Setting MaxRetries to 0 disables retries.
//
// Backoff for attempt n is InitialBackoff * Multiplier^n, clamped to MaxBackoff,
// then perturbed by up to ±Jitter. Jitter is symmetric; a value of 0.2 yields
// delays in [80%, 120%] of the base.
type RetryPolicy struct {
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Multiplier     float64
	Jitter         float64
}

// DefaultRetryPolicy is the policy used when none is configured.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries:     3,
		InitialBackoff: 200 * time.Millisecond,
		MaxBackoff:     5 * time.Second,
		Multiplier:     2.0,
		Jitter:         0.2,
	}
}

// nextDelay is the backoff applied before the n-th retry (n starts at 0 for
// the first retry, i.e. between attempt 1 and attempt 2).
func (p RetryPolicy) nextDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := float64(p.InitialBackoff) * math.Pow(p.Multiplier, float64(attempt))
	if base > float64(p.MaxBackoff) {
		base = float64(p.MaxBackoff)
	}
	if p.Jitter > 0 {
		// Symmetric jitter in [-Jitter, +Jitter]. Jitter is always well
		// below 1, so the result stays positive.
		delta := (rand.Float64()*2 - 1) * p.Jitter
		base *= 1 + delta
	}
	return time.Duration(base)
}

// statusOverloaded is the non-standard status the decision API uses for
// "temporarily overloaded". It is documented next to 429 and is as transient.
const statusOverloaded = 529

// isRetryableStatus reports whether the given HTTP status warrants a retry.
// We retry rate limiting (429), the standard transient server failures, and
// the API's own overload status (529).
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		statusOverloaded:
		return true
	}
	return false
}

// waitDelay is how long to wait before the next attempt. It caps the
// server's Retry-After so one request can never pin a goroutine for
// minutes.
func waitDelay(d time.Duration) time.Duration {
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// parseRetryAfter reads a Retry-After header. Servers use either a count of
// seconds or an HTTP date.
//
// Returns 0 when the header is absent, unparseable, or asks for a wait of
// zero or less. The value is what the server asked for: the client caps how
// long it waits, so one request can never pin a goroutine for minutes, but
// the caller still sees what the server said.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		if secs > math.MaxInt64/int64(time.Second) {
			// Larger than any duration can hold; report the maximum rather
			// than overflowing the multiply below.
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// isContextErr reports whether the error (or anything it wraps) is a context
// cancellation or deadline expiry. Context errors terminate retries.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
