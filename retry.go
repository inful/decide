package decide

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
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
		// Symmetric jitter in [-Jitter, +Jitter].
		delta := (rand.Float64()*2 - 1) * p.Jitter
		base *= 1 + delta
	}
	if base < 0 {
		base = 0
	}
	return time.Duration(base)
}

// isRetryableStatus reports whether the given HTTP status warrants a retry.
// We retry rate limiting (429) and the standard transient server failures.
func isRetryableStatus(code int) bool {
	switch code {
	case httpTooManyRequests, httpInternalServerError, httpBadGateway, httpServiceUnavailable, httpGatewayTimeout:
		return true
	}
	return false
}

// isContextErr reports whether the error (or anything it wraps) is a context
// cancellation or deadline expiry. Context errors terminate retries.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Status code constants duplicated from net/http so we can keep retry.go
// dependency-free in spirit (no need to import net/http here).
const (
	httpTooManyRequests       = 429
	httpInternalServerError   = 500
	httpBadGateway            = 502
	httpServiceUnavailable    = 503
	httpGatewayTimeout        = 504
)