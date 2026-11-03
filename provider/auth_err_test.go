// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// E33 (founder live test 2026-10-28): a rejected API key is as permanent as a
// 400 — the same key gets the same answer — but the orchestrator retried it up
// to its iteration budget, 2 s apart, so the owner waited minutes to learn the
// key was wrong. IsAuthErr is the classifier it now fails fast on.
func TestIsAuthErr(t *testing.T) {
	yes := map[string]error{
		"claude 401 typed":   fmt.Errorf("claude-agentic: authentication_error (HTTP 401): invalid x-api-key"),
		"claude 401 raw":     fmt.Errorf("claude-agentic stream: HTTP 401: {}"),
		"claude 403 typed":   fmt.Errorf("claude-agentic: permission_error (HTTP 403): no access to model"),
		"openai 401":         newOpenAIHTTPError("openai", "status", 401, http.Header{}, "Incorrect API key provided"),
		"openai 403 wrapped": fmt.Errorf("openai: stream request: %w", newOpenAIHTTPError("openai", "stream status", 403, http.Header{}, "forbidden")),
	}
	for name, err := range yes {
		if !IsAuthErr(err) {
			t.Errorf("%s: IsAuthErr = false, want true", name)
		}
	}
	no := map[string]error{
		"nil":            nil,
		"claude 400":     fmt.Errorf("claude-agentic: invalid_request_error (HTTP 400): bad media type"),
		"claude 429":     fmt.Errorf("claude-agentic: rate_limit_error (HTTP 429): slow down"),
		"claude 529":     fmt.Errorf("claude-agentic stream: overloaded_error (HTTP 529): overloaded"),
		"openai 500":     newOpenAIHTTPError("openai", "status", 500, http.Header{}, "oops"),
		"transport":      errors.New("claude-agentic: request failed: connection reset by peer"),
		"4010 lookalike": fmt.Errorf("claude-agentic: HTTP 4010: nonsense"),
	}
	for name, err := range no {
		if IsAuthErr(err) {
			t.Errorf("%s: IsAuthErr = true, want false", name)
		}
	}
}
