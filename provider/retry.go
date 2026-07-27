package provider

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Phase 3 item G — robust retry / backoff / model fallback.
//
// The non-streaming and streaming paths both feed network/API errors back to
// the orchestrator's runLoop, which currently retries ONCE with a fixed 2-second
// sleep. That misses three opportunities:
//
//   1. The Anthropic API regularly returns 429 / 529 under load with a
//      Retry-After header that should be honoured.
//   2. Transient 5xx and network errors are recoverable; fatal 4xx (e.g.
//      authentication / bad request) are not — they should fail fast.
//   3. Sustained pressure on the primary model is sometimes worth a one-shot
//      fallback to a secondary model so the user gets *some* answer.
//
// This file encapsulates the retry policy. CompleteWithTools and
// CompleteWithToolsStream wrap their underlying single-shot HTTP call with
// `retryWithPolicy` so the policy applies to both paths.

// RetryPolicy controls how transient errors are retried. The zero value
// disables retries entirely (single attempt only); production code should
// construct one via DefaultRetryPolicy() and customise from there.
type RetryPolicy struct {
	// MaxAttempts is the upper bound on total attempts (including the first).
	// 1 means "try once, no retries". Default 5.
	MaxAttempts int

	// BaseDelay is the starting backoff window. Each subsequent attempt
	// doubles up to MaxDelay. Default 1 second.
	BaseDelay time.Duration

	// MaxDelay caps the backoff between attempts. Default 30 seconds.
	MaxDelay time.Duration

	// Jitter adds randomised fuzz to each backoff so synchronised clients
	// don't herd. Expressed as a fraction of the computed delay; 0.2 means
	// ±20%. Default 0.2.
	Jitter float64

	// HonourRetryAfter, when true, overrides the computed backoff with the
	// server-sent Retry-After header (if shorter than MaxDelay). Default true.
	HonourRetryAfter bool
}

// DefaultRetryPolicy returns the tuned defaults used in production. Callers
// can customise fields without re-declaring the whole struct.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:      5,
		BaseDelay:        time.Second,
		MaxDelay:         30 * time.Second,
		Jitter:           0.2,
		HonourRetryAfter: true,
	}
}

// retryClassification labels a single attempt's outcome.
type retryClassification int

const (
	classSuccess retryClassification = iota
	classTransient
	classFatal
)

// retryableError is returned from an attempt when the caller already knows
// the error is transient and wants to bypass the HTTP-status classifier
// (e.g. an SSE parse error mid-stream).
type retryableError struct{ err error }

func (r *retryableError) Error() string { return r.err.Error() }
func (r *retryableError) Unwrap() error { return r.err }

// retryable wraps an error so retryWithPolicy treats it as transient.
func retryable(err error) error {
	if err == nil {
		return nil
	}
	return &retryableError{err: err}
}

// classifyHTTPError decides whether an HTTP response should be retried.
// 429, 529, and 5xx are transient; 4xx (except 408 / 429) are fatal.
//
// The function also extracts a server-sent Retry-After header (seconds or
// HTTP-date) so the caller can honour the wait. Returns (class, retryAfter).
func classifyHTTPError(statusCode int, header http.Header) (retryClassification, time.Duration) {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return classSuccess, 0
	case statusCode == 408, statusCode == 429, statusCode == 529:
		return classTransient, parseRetryAfter(header.Get("Retry-After"))
	case statusCode >= 500:
		return classTransient, parseRetryAfter(header.Get("Retry-After"))
	default:
		// Everything else (400, 401, 403, 404, ...) is a hard fail.
		return classFatal, 0
	}
}

// classifyTransportError decides whether a Go-side transport error (no HTTP
// response received) is worth retrying. Network errors are transient;
// context cancellation is fatal.
func classifyTransportError(err error) retryClassification {
	if err == nil {
		return classSuccess
	}
	// Explicitly-marked transient errors win.
	var re *retryableError
	if errors.As(err, &re) {
		return classTransient
	}
	// Context errors should NOT be retried — the caller is gone.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return classFatal
	}
	// Common transient network errors.
	var nerr net.Error
	if errors.As(err, &nerr) {
		if nerr.Timeout() {
			return classTransient
		}
	}
	// String-match common patterns from net/http for unwrapped errors.
	msg := err.Error()
	for _, m := range []string{
		"connection reset",
		"connection refused",
		"EOF",
		"timeout",
		"temporary failure",
		"i/o timeout",
		"network is unreachable",
	} {
		if strings.Contains(msg, m) {
			return classTransient
		}
	}
	return classFatal
}

// parseRetryAfter accepts the two RFC 7231 forms: a delta-seconds integer or
// an HTTP-date. Anything unrecognised yields 0.
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// backoffFor returns the wait duration for the given attempt number (1-based).
// Honours Retry-After when present and shorter than MaxDelay; otherwise
// applies exponential backoff with jitter.
func (p RetryPolicy) backoffFor(attempt int, retryAfter time.Duration) time.Duration {
	if p.HonourRetryAfter && retryAfter > 0 {
		if p.MaxDelay > 0 && retryAfter > p.MaxDelay {
			return p.MaxDelay
		}
		return retryAfter
	}
	base := p.BaseDelay
	if base <= 0 {
		base = time.Second
	}
	// 1-based: attempt 1 gets BaseDelay, attempt 2 gets 2*BaseDelay, etc.
	shift := attempt - 1
	if shift < 0 {
		shift = 0
	}
	delay := base << shift
	if p.MaxDelay > 0 && delay > p.MaxDelay {
		delay = p.MaxDelay
	}
	if p.Jitter > 0 {
		// Symmetric jitter: ± (Jitter * delay).
		span := float64(delay) * p.Jitter
		jitter := time.Duration(rand.Float64()*2*span - span)
		delay += jitter
		if delay < 0 {
			delay = 0
		}
	}
	return delay
}

// attemptFunc is the per-attempt closure. It returns either:
//   - (resp, nil, success):  the operation completed.
//   - (resp, nil, transient) or (nil, err, transient): a retry-worthy failure.
//   - (nil, err, fatal): a non-retryable failure.
//
// The retryAfter return surfaces the server's suggestion when the failure
// came from an HTTP status with that header.
type attemptFunc[T any] func(ctx context.Context, attempt int) (result T, classification retryClassification, retryAfter time.Duration, err error)

// retryWithPolicy runs `attempt` up to policy.MaxAttempts times, sleeping
// between attempts according to backoffFor. Returns the first success, the
// first fatal error, or the last error if attempts are exhausted.
//
// The context cancellation is honoured during sleeps so a parent cancel
// unblocks the retry promptly.
func retryWithPolicy[T any](ctx context.Context, policy RetryPolicy, attempt attemptFunc[T]) (T, error) {
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = 1
	}
	var (
		zero    T
		lastErr error
	)
	for i := 1; i <= policy.MaxAttempts; i++ {
		if err := ctx.Err(); err != nil {
			if lastErr == nil {
				lastErr = err
			}
			return zero, lastErr
		}
		result, class, retryAfter, err := attempt(ctx, i)
		switch class {
		case classSuccess:
			return result, nil
		case classFatal:
			if err == nil {
				err = errors.New("provider: fatal error without details")
			}
			return zero, err
		case classTransient:
			lastErr = err
			if i == policy.MaxAttempts {
				break
			}
			delay := policy.backoffFor(i, retryAfter)
			select {
			case <-ctx.Done():
				if lastErr == nil {
					lastErr = ctx.Err()
				}
				return zero, lastErr
			case <-time.After(delay):
				// retry
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("provider: exhausted %d attempts without classification", policy.MaxAttempts)
	}
	return zero, lastErr
}

// ModelFallback configures an optional one-shot fallback when the primary
// model's retry budget is exhausted. The secondary model is tried once with
// the same arguments before returning the final error to the caller.
//
// Set Secondary to "" to disable the fallback (default).
type ModelFallback struct {
	Secondary string // e.g. "claude-3-5-haiku-20241022"
}
