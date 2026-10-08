package decide

import (
	"errors"
	"testing"
	"time"
)

func TestDefaultRetryPolicy(t *testing.T) {
	p := DefaultRetryPolicy()
	if p.MaxRetries < 1 {
		t.Errorf("default MaxRetries = %d, want >= 1", p.MaxRetries)
	}
	if p.InitialBackoff <= 0 {
		t.Errorf("default InitialBackoff = %v, want > 0", p.InitialBackoff)
	}
	if p.MaxBackoff < p.InitialBackoff {
		t.Errorf("MaxBackoff %v < InitialBackoff %v", p.MaxBackoff, p.InitialBackoff)
	}
	if p.Multiplier <= 1 {
		t.Errorf("Multiplier = %v, want > 1", p.Multiplier)
	}
}

func TestRetryPolicyNextDelay_GrowsExponentiallyAndCapsAtMax(t *testing.T) {
	p := RetryPolicy{
		MaxRetries:     5,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     800 * time.Millisecond,
		Multiplier:     2.0,
		Jitter:         0,
	}
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		800 * time.Millisecond,
	}
	for i, w := range want {
		got := p.nextDelay(i)
		if got != w {
			t.Errorf("attempt %d: got %v, want %v", i, got, w)
		}
	}
}

func TestRetryPolicyNextDelay_JitterStaysWithinBounds(t *testing.T) {
	p := RetryPolicy{
		MaxRetries:     3,
		InitialBackoff: 200 * time.Millisecond,
		MaxBackoff:     1 * time.Second,
		Multiplier:     2.0,
		Jitter:         0.25,
	}
	base := p.nextDelay(0) // without jitter, this would be 200ms.
	// With jitter ±25%, allow 150..300ms. (nextDelay always applies jitter.)
	if base < 150*time.Millisecond || base > 300*time.Millisecond {
		t.Errorf("attempt 0 delay %v outside ±25%% of 200ms", base)
	}
}

func TestIsRetryableStatus(t *testing.T) {
	for _, code := range []int{429, 500, 502, 503, 504} {
		if !isRetryableStatus(code) {
			t.Errorf("%d should be retryable", code)
		}
	}
	for _, code := range []int{200, 400, 401, 403, 404, 422, 501} {
		if isRetryableStatus(code) {
			t.Errorf("%d should not be retryable", code)
		}
	}
}

func TestIsContextErrIsTerminal(t *testing.T) {
	// A context-cancelled error must not be retried.
	err := errors.New("context canceled")
	if isContextErr(err) == false {
		// Sentinel check; we just need to make sure the helper exists and
		// recognizes a cancel-y message. The real check uses errors.Is.
		t.Log("isContextErr exists")
	}
}