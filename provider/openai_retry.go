// SPDX-License-Identifier: Apache-2.0

package provider

// openai_retry.go — retry/backoff for the OpenAI-compatible provider.
//
// The Claude provider has carried a RetryPolicy since Phase 3 G ("transient
// API errors (429, 529, 5xx, network)"); the OpenAI provider carried none, so
// the same transient failure that Anthropic rides out killed a turn outright:
//
//	[LLM error: openai: stream request: Post ".../v1/responses": EOF]
//
// A bare EOF on POST is the stale-keep-alive race — the server closes a pooled
// idle connection just as the request is written. classifyTransportError
// already calls that transient; nothing was asking it. The same gap dropped
// 429s and 5xx on the floor, with the server's Retry-After unread.
//
// Two things this deliberately does NOT do:
//
//   - It does not nest. The staged degrade inside doStream/doResponsesStream
//     ends by calling the non-streaming path, so the entry points wrap the
//     RAW transport (doComplete / doResponses) rather than the retrying
//     CompleteWithTools. One retry budget per public call, not budget².
//
//   - It does not replay text the user has already seen. A stream that fails
//     after emitting deltas is NOT retried, because the transcript the user is
//     reading would gain a second copy of the same partial answer. Retrying is
//     only safe while nothing has been committed to the screen — which is
//     exactly the window the reported EOF failed in, since it never got a
//     response at all.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// openaiHTTPError carries the status and Retry-After of a non-2xx reply so the
// retry policy can classify it the way the Claude path does. Error() reproduces
// the message the transport used to format inline, so existing error text — and
// everything matching on it — is unchanged.
type openaiHTTPError struct {
	status     int
	retryAfter time.Duration
	msg        string
}

func (e *openaiHTTPError) Error() string { return e.msg }

// Status exposes the HTTP status for callers that need to branch on it.
func (e *openaiHTTPError) Status() int { return e.status }

// newOpenAIHTTPError builds the typed error for a non-2xx reply. label is
// "status" or "stream status", keeping the historic
// "<name>: <label> <code>: <body>" message shape byte-identical.
func newOpenAIHTTPError(name, label string, status int, header http.Header, body string) *openaiHTTPError {
	return &openaiHTTPError{
		status:     status,
		retryAfter: parseRetryAfter(header.Get("Retry-After")),
		msg:        fmt.Sprintf("%s: %s %d: %s", name, label, status, strings.TrimSpace(body)),
	}
}

// classifyOpenAIError maps a failed attempt onto the retry classification and
// the server's suggested wait. An HTTP reply is classified by status; anything
// else is a transport error (EOF, reset, timeout, …).
func classifyOpenAIError(err error) (retryClassification, time.Duration) {
	if err == nil {
		return classSuccess, 0
	}
	var he *openaiHTTPError
	if errors.As(err, &he) {
		// The header was already read for Retry-After at construction; pass nil
		// and use the stored value.
		class, _ := classifyHTTPError(he.status, nil)
		return class, he.retryAfter
	}
	return classifyTransportError(err), 0
}

// withRetry runs a non-streaming attempt under the provider's retry policy.
func (c *OpenAIAgenticProvider) withRetry(
	ctx context.Context,
	attempt func(ctx context.Context) (*AgenticResponse, error),
) (*AgenticResponse, error) {
	return retryWithPolicy(ctx, c.retryPolicy, func(ctx context.Context, _ int) (*AgenticResponse, retryClassification, time.Duration, error) {
		resp, err := attempt(ctx)
		if err == nil {
			return resp, classSuccess, 0, nil
		}
		class, after := classifyOpenAIError(err)
		return nil, class, after, err
	})
}

// withStreamRetry runs a streaming attempt under the provider's retry policy,
// abandoning retries the moment any text has reached the caller's callback.
//
// The guard is the difference between fixing a dropped connection and showing
// the user half an answer twice. It watches text deltas specifically: those are
// the events the orchestrator forwards to the pane, so they are what a replay
// would visibly duplicate.
func (c *OpenAIAgenticProvider) withStreamRetry(
	ctx context.Context,
	cb StreamCallback,
	attempt func(ctx context.Context, cb StreamCallback) (*AgenticResponse, error),
) (*AgenticResponse, error) {
	committed := false
	guarded := cb
	if cb != nil {
		guarded = func(ev StreamEvent) {
			if ev.Type == StreamEventTextDelta && ev.Text != "" {
				committed = true
			}
			cb(ev)
		}
	}
	return retryWithPolicy(ctx, c.retryPolicy, func(ctx context.Context, _ int) (*AgenticResponse, retryClassification, time.Duration, error) {
		resp, err := attempt(ctx, guarded)
		if err == nil {
			return resp, classSuccess, 0, nil
		}
		if committed {
			// Text is already on the user's screen. Whatever went wrong, a
			// second attempt would re-stream it; surface the error instead.
			return nil, classFatal, 0, err
		}
		class, after := classifyOpenAIError(err)
		return nil, class, after, err
	})
}

// SetRetryPolicy installs a custom retry/backoff policy. Pass
// RetryPolicy{MaxAttempts: 1} to disable retries entirely. Mirrors the
// Claude provider's setter.
func (c *OpenAIAgenticProvider) SetRetryPolicy(p RetryPolicy) {
	c.retryPolicy = p
}
