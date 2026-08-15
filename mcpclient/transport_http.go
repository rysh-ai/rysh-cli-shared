// SPDX-License-Identifier: Apache-2.0

package mcpclient

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
	"sync"
)

// maxRedirects bounds the redirect chain. Every hop is re-guarded before it is
// followed, so a 30x cannot smuggle a request onto an address the guard would
// have refused — the same rule chatbot_http_tool.go applies to owner-configured
// HTTP tools.
const maxRedirects = 5

// guardError marks a refusal that came from the egress policy rather than from
// the network, so Execute can classify it as permission_denied instead of a
// retryable transient failure.
type guardError struct{ msg string }

func (e *guardError) Error() string { return e.msg }

func isGuardErr(err error) bool {
	var g *guardError
	return errors.As(err, &g)
}

// sizeError marks a response that exceeded the configured body cap.
type sizeError struct{ msg string }

func (e *sizeError) Error() string { return e.msg }

// httpTransport implements the MCP "Streamable HTTP" transport: each JSON-RPC
// message is POSTed to the server's endpoint and the reply arrives either as a
// single application/json body or as a text/event-stream (SSE) frame carrying
// one JSON-RPC message. The server may assign an Mcp-Session-Id on initialize,
// which the client echoes on every subsequent request.
//
// There is no long-lived GET stream, so server→client notifications
// (notifications/tools/list_changed) are not delivered. A server-side consumer
// re-lists on its own schedule instead of reacting to a push.
type httpTransport struct {
	name     string
	endpoint *url.URL
	headers  map[string]string
	guard    URLGuard
	client   *http.Client
	maxBody  int64

	mu        sync.Mutex
	sessionID string
}

// roundTrip sends a request carrying an ID and returns the matching response.
// A JSON-RPC error object is returned as the error (and also in the message) so
// callers can inspect the code.
func (t *httpTransport) roundTrip(ctx context.Context, req jsonrpcRequest) (*jsonrpcMessage, error) {
	req.JSONRPC = "2.0"
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	data, ct, err := t.post(ctx, body)
	if err != nil {
		return nil, err
	}
	msg, err := parseHTTPResult(ct, data, string(req.ID))
	if err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, fmt.Errorf("mcp server %q returned no response for %s", t.name, req.Method)
	}
	if msg.Error != nil {
		return msg, msg.Error
	}
	return msg, nil
}

// notify sends a fire-and-forget notification (no ID, no response expected).
func (t *httpTransport) notify(ctx context.Context, req jsonrpcRequest) error {
	req.JSONRPC = "2.0"
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}
	_, _, err = t.post(ctx, body)
	return err
}

// post sends one JSON-RPC payload and returns the raw response body plus its
// Content-Type. The endpoint is re-authorized by the guard on every call — not
// only at construction — so a host that starts resolving to an internal address
// after the client was built is refused on the next request rather than
// trusted for the client's lifetime.
func (t *httpTransport) post(ctx context.Context, body []byte) ([]byte, string, error) {
	if err := t.guard(ctx, t.endpoint); err != nil {
		if isGuardErr(err) {
			return nil, "", err
		}
		return nil, "", &guardError{msg: fmt.Sprintf("mcp server %q refused by egress policy: %v", t.name, err)}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Streamable HTTP requires the client to accept both response framings.
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		httpReq.Header.Set(k, v)
	}
	t.mu.Lock()
	if t.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", t.sessionID)
	}
	t.mu.Unlock()

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("reach mcp server %q: %w", t.name, err)
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}

	// Read one byte past the cap so an oversized body is detected rather than
	// silently truncated into a half-parsed JSON-RPC message.
	data, err := io.ReadAll(io.LimitReader(resp.Body, t.maxBody+1))
	if err != nil {
		return nil, "", fmt.Errorf("read mcp server %q response: %w", t.name, err)
	}
	if int64(len(data)) > t.maxBody {
		return nil, "", &sizeError{msg: fmt.Sprintf(
			"mcp server %q response too large (over %d bytes)", t.name, t.maxBody)}
	}

	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return nil, ct, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("mcp server %q returned HTTP %d: %s",
			t.name, resp.StatusCode, truncate(string(data), 512))
	}
	return data, ct, nil
}

func (t *httpTransport) close() error {
	t.client.CloseIdleConnections()
	return nil
}

// guardedRedirectChecker re-applies the egress policy to every redirect hop and
// bounds the chain length.
func guardedRedirectChecker(guard URLGuard, limit int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= limit {
			return fmt.Errorf("stopped after %d redirects", limit)
		}
		if err := guard(req.Context(), req.URL); err != nil {
			return &guardError{msg: fmt.Sprintf("redirect to %s refused by egress policy: %v", req.URL.Redacted(), err)}
		}
		return nil
	}
}

// parseHTTPResult decodes a JSON-RPC response from either an application/json
// body or an SSE body. For SSE it returns the message whose id matches wantID,
// falling back to the first response found.
func parseHTTPResult(contentType string, body []byte, wantID string) (*jsonrpcMessage, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSE(body, wantID)
	}
	if body[0] == '[' {
		var batch []jsonrpcMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, fmt.Errorf("parse JSON-RPC batch: %w", err)
		}
		return pickMessage(batch, wantID), nil
	}
	var m jsonrpcMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse JSON-RPC response: %w", err)
	}
	return &m, nil
}

// parseSSE extracts JSON-RPC messages from Server-Sent Events frames. Each
// event accumulates one or more "data:" lines; a blank line terminates it.
func parseSSE(body []byte, wantID string) (*jsonrpcMessage, error) {
	var msgs []jsonrpcMessage
	var data strings.Builder
	flush := func() {
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" {
			return
		}
		var m jsonrpcMessage
		if err := json.Unmarshal([]byte(payload), &m); err == nil {
			msgs = append(msgs, m)
		}
	}
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimRight(raw, "\r")
		switch {
		case line == "":
			flush()
		// Every other line — SSE ":" comments, "event:", keep-alives — is
		// ignored; only blank lines and data: lines are meaningful here.
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	if len(msgs) == 0 {
		return nil, fmt.Errorf("no JSON-RPC message in event stream")
	}
	return pickMessage(msgs, wantID), nil
}

// pickMessage selects the response matching wantID, else the first
// non-notification message.
func pickMessage(msgs []jsonrpcMessage, wantID string) *jsonrpcMessage {
	for i := range msgs {
		if string(msgs[i].ID) == wantID && msgs[i].Method == "" {
			return &msgs[i]
		}
	}
	for i := range msgs {
		if msgs[i].Method == "" {
			return &msgs[i]
		}
	}
	return &msgs[0]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
