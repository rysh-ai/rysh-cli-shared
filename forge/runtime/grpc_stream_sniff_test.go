// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Response dispatch is by Content-Type, whatever protocol the request used: a
// body labelled application/connect+json is Connect envelopes and must never be
// split on newlines as NDJSON (which yields binary garbage frames). These tests
// use only the pre-existing API on purpose, so they run unchanged at the base.

func sniffEnvelope(flag byte, payload string) []byte {
	b := make([]byte, 5, 5+len(payload))
	b[0] = flag
	binary.BigEndian.PutUint32(b[1:], uint32(len(payload)))
	return append(b, payload...)
}

// TestGatewayModeDecodesConnectLabelledResponse: default (gateway) mode, the
// server answers with Connect framing → frames decode cleanly.
func TestGatewayModeDecodesConnectLabelledResponse(t *testing.T) {
	var body []byte
	body = append(body, sniffEnvelope(0x00, `{"text":"one"}`)...)
	body = append(body, sniffEnvelope(0x00, `{"text":"two"}`)...)
	body = append(body, sniffEnvelope(0x02, `{}`)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	exec := NewHTTPExecutor(srv.URL, nil, Credential{}, Options{})
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()
	id, err := exec.StartServerStream(m, watchOp(), nil)
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	frames, res := waitFrames(t, m, id, 2, true)
	if len(frames) != 2 || !strings.Contains(frames[0], `"one"`) || !strings.Contains(frames[1], `"two"`) {
		t.Fatalf("frames = %q, want the two decoded Connect messages", frames)
	}
	for _, f := range frames {
		if strings.ContainsRune(f, 0) {
			t.Fatalf("frame %q carries raw envelope bytes: parsed as NDJSON", f)
		}
	}
	if res.Reason != "stream ended" {
		t.Fatalf("end reason = %q, want a clean end", res.Reason)
	}
}

// TestConnectProtoResponseRefused: we speak only the JSON codec; a
// connect+proto stream fails loudly instead of being pushed as binary.
func TestConnectProtoResponseRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+proto")
		_, _ = w.Write(sniffEnvelope(0x00, "\x0a\x03one"))
	}))
	defer srv.Close()

	exec := NewHTTPExecutor(srv.URL, nil, Credential{}, Options{})
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()
	id, err := exec.StartServerStream(m, watchOp(), nil)
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	frames, res := waitFrames(t, m, id, 0, true)
	if len(frames) != 0 || !strings.Contains(res.Reason, "connect+proto") {
		t.Fatalf("frames=%q reason=%q, want a refusal naming the unsupported codec", frames, res.Reason)
	}
}
