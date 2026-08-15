// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestClassifyHTTPError pins the retry-vs-fatal decisions for each HTTP
// status range. 408 / 429 / 5xx / 529 are transient; everything else (200
// excluded) is fatal.
func TestClassifyHTTPError(t *testing.T) {
	cases := []struct {
		status int
		want   retryClassification
	}{
		{200, classSuccess},
		{299, classSuccess},
		{300, classFatal},
		{400, classFatal},
		{401, classFatal},
		{403, classFatal},
		{404, classFatal},
		{408, classTransient}, // request timeout
		{409, classFatal},
		{429, classTransient}, // rate limit
		{499, classFatal},
		{500, classTransient},
		{503, classTransient},
		{529, classTransient}, // Anthropic "overloaded"
		{599, classTransient},
	}
	for _, c := range cases {
		got, _ := classifyHTTPError(c.status, nil)
		if got != c.want {
			t.Errorf("status %d → %v, want %v", c.status, got, c.want)
		}
	}
}

// TestClassifyHTTPError_RetryAfter parses the Retry-After header in both
// seconds-int and HTTP-date forms.
func TestClassifyHTTPError_RetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "7")
	_, d := classifyHTTPError(429, h)
	if d != 7*time.Second {
		t.Errorf("integer Retry-After: got %v", d)
	}
	// HTTP-date form: 10 seconds in the future.
	future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	h.Set("Retry-After", future)
	_, d = classifyHTTPError(529, h)
	if d <= 0 || d > 15*time.Second {
		t.Errorf("HTTP-date Retry-After: got %v", d)
	}
	// Garbage values yield 0.
	h.Set("Retry-After", "not-a-thing")
	_, d = classifyHTTPError(429, h)
	if d != 0 {
		t.Errorf("garbage Retry-After: got %v", d)
	}
}

// TestClassifyTransportError covers the transport-error path: context errors
// are fatal; net.Error{Timeout()=true} and common reset/refused strings are
// transient.
func TestClassifyTransportError(t *testing.T) {
	if got := classifyTransportError(nil); got != classSuccess {
		t.Errorf("nil err = %v", got)
	}
	if got := classifyTransportError(context.Canceled); got != classFatal {
		t.Errorf("Canceled = %v", got)
	}
	if got := classifyTransportError(context.DeadlineExceeded); got != classFatal {
		t.Errorf("DeadlineExceeded = %v", got)
	}
	// Wrapped retryable.
	if got := classifyTransportError(retryable(errors.New("eh"))); got != classTransient {
		t.Errorf("retryable() should be transient, got %v", got)
	}
	// Plain string match patterns.
	for _, s := range []string{"connection reset", "connection refused", "EOF", "i/o timeout"} {
		if got := classifyTransportError(errors.New(s)); got != classTransient {
			t.Errorf("%q expected transient, got %v", s, got)
		}
	}
	// Anything else is fatal.
	if got := classifyTransportError(errors.New("oh no something else")); got != classFatal {
		t.Errorf("unknown error should be fatal, got %v", got)
	}
}

// fakeNetError satisfies net.Error so we can exercise the Timeout() path.
type fakeNetError struct{ to bool }

func (f *fakeNetError) Error() string   { return "fake net error" }
func (f *fakeNetError) Timeout() bool   { return f.to }
func (f *fakeNetError) Temporary() bool { return false }

var _ net.Error = (*fakeNetError)(nil)

// TestClassifyTransportError_NetTimeout: a net.Error whose Timeout() is true
// should be classified transient.
func TestClassifyTransportError_NetTimeout(t *testing.T) {
	if got := classifyTransportError(&fakeNetError{to: true}); got != classTransient {
		t.Errorf("net.Error timeout should be transient, got %v", got)
	}
	if got := classifyTransportError(&fakeNetError{to: false}); got != classFatal {
		t.Errorf("net.Error non-timeout should be fatal (no known string match), got %v", got)
	}
}

// TestRetryWithPolicy_HonorRetryAfter verifies that a server-supplied
// Retry-After overrides exponential backoff (when shorter than MaxDelay).
func TestRetryWithPolicy_HonorRetryAfter(t *testing.T) {
	policy := RetryPolicy{
		MaxAttempts:      3,
		BaseDelay:        500 * time.Millisecond,
		MaxDelay:         5 * time.Second,
		Jitter:           0,
		HonourRetryAfter: true,
	}
	d := policy.backoffFor(1, 30*time.Millisecond)
	if d != 30*time.Millisecond {
		t.Errorf("expected Retry-After to override; got %v", d)
	}
	// Retry-After longer than MaxDelay → capped.
	d = policy.backoffFor(1, 1*time.Minute)
	if d != 5*time.Second {
		t.Errorf("expected MaxDelay cap; got %v", d)
	}
}

// TestRetryWithPolicy_ExponentialBackoff sanity-checks the per-attempt
// growth without jitter.
func TestRetryWithPolicy_ExponentialBackoff(t *testing.T) {
	p := RetryPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: 10 * time.Second, Jitter: 0}
	got1 := p.backoffFor(1, 0)
	got2 := p.backoffFor(2, 0)
	got3 := p.backoffFor(3, 0)
	if got1 != 100*time.Millisecond || got2 != 200*time.Millisecond || got3 != 400*time.Millisecond {
		t.Errorf("backoff sequence wrong: %v %v %v", got1, got2, got3)
	}
	// Cap kicks in.
	cap := p.backoffFor(20, 0)
	if cap != 10*time.Second {
		t.Errorf("expected MaxDelay cap at attempt 20, got %v", cap)
	}
}

// TestRetryWithPolicy_StopsOnSuccess: an attempt returning classSuccess
// breaks out without further attempts.
func TestRetryWithPolicy_StopsOnSuccess(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	attempts := 0
	result, err := retryWithPolicy(context.Background(), p, func(_ context.Context, n int) (int, retryClassification, time.Duration, error) {
		attempts++
		if n == 2 {
			return 99, classSuccess, 0, nil
		}
		return 0, classTransient, 0, errors.New("nope")
	})
	if err != nil || result != 99 || attempts != 2 {
		t.Errorf("result=%d err=%v attempts=%d", result, err, attempts)
	}
}

// TestRetryWithPolicy_StopsOnFatal: classFatal does not get retried.
func TestRetryWithPolicy_StopsOnFatal(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	attempts := 0
	_, err := retryWithPolicy(context.Background(), p, func(_ context.Context, _ int) (int, retryClassification, time.Duration, error) {
		attempts++
		return 0, classFatal, 0, errors.New("bad request")
	})
	if err == nil || attempts != 1 {
		t.Errorf("expected fatal stop with 1 attempt, got %d attempts err=%v", attempts, err)
	}
}

// TestRetryWithPolicy_ExhaustsAttempts: when every attempt is transient,
// we run MaxAttempts times and return the last error.
func TestRetryWithPolicy_ExhaustsAttempts(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	attempts := 0
	_, err := retryWithPolicy(context.Background(), p, func(_ context.Context, _ int) (int, retryClassification, time.Duration, error) {
		attempts++
		return 0, classTransient, 0, errors.New("nope")
	})
	if err == nil || attempts != 3 {
		t.Errorf("expected exhaust: 3 attempts, err non-nil; got attempts=%d err=%v", attempts, err)
	}
}

// TestRetryWithPolicy_ContextCancel: a cancellation during a retry sleep
// unblocks the retry and returns the cancellation error.
func TestRetryWithPolicy_ContextCancel(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, BaseDelay: 500 * time.Millisecond, MaxDelay: 500 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the first attempt's sleep starts.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := retryWithPolicy(ctx, p, func(_ context.Context, _ int) (int, retryClassification, time.Duration, error) {
		return 0, classTransient, 0, errors.New("transient")
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Errorf("expected error on cancel")
	}
	if elapsed > 300*time.Millisecond {
		t.Errorf("cancel should have unblocked sleep, took %v", elapsed)
	}
}
