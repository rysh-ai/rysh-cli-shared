// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"fmt"
	"strings"
	"testing"
)

// E33: the run loop stops retrying on errors that are permanent for the run —
// a 400 (unchanged) and now a rejected API key (401/403).
func TestNonRetryableNotice(t *testing.T) {
	notice, ok := nonRetryableNotice(fmt.Errorf("claude-agentic: authentication_error (HTTP 401): invalid x-api-key"))
	if !ok {
		t.Fatal("a 401 is retried; it must end the run at once")
	}
	for _, want := range []string{"HTTP 401/403", "not retrying", "API key"} {
		if !strings.Contains(notice, want) {
			t.Errorf("401 notice %q does not contain %q", notice, want)
		}
	}

	notice, ok = nonRetryableNotice(fmt.Errorf("claude-agentic: invalid_request_error (HTTP 400): bad media type"))
	if !ok || notice != "[request rejected as invalid (HTTP 400) — not retrying; fix the request payload and resume]\n" {
		t.Errorf("400: got (%q, %v), want the unchanged 400 notice", notice, ok)
	}

	for _, err := range []error{
		fmt.Errorf("claude-agentic: rate_limit_error (HTTP 429): slow down"),
		fmt.Errorf("claude-agentic: HTTP 500: oops"),
		fmt.Errorf("claude-agentic: request failed: connection reset by peer"),
	} {
		if _, ok := nonRetryableNotice(err); ok {
			t.Errorf("%v: treated as permanent; transient errors must still be retried", err)
		}
	}
}
