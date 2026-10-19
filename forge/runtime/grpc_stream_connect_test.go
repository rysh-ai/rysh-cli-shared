// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"
)

// Connect enveloped streaming framing (E23 T2, design 015 §5.3): with
// Options.StreamProtocol = StreamProtocolConnect the executor sends the request
// as one Connect envelope and decodes enveloped response frames, including the
// end-of-stream message's error. The first two tests run against a REAL
// connect-go handler, not a hand-rolled fixture; the malformed-frame tests need
// byte-level control a conforming server cannot give, so they write raw bytes.

const feedWatchProcedure = "/feed.Feed/Watch"

// newConnectFeedServer mounts a real connect-go server-streaming handler at the
// op's wire path. Struct request/response avoids protoc codegen; connect-go
// serves the JSON codec (application/connect+json) for them out of the box.
func newConnectFeedServer(t *testing.T, fn func(context.Context, *connect.Request[structpb.Struct], *connect.ServerStream[structpb.Struct]) error) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(feedWatchProcedure, connect.NewServerStreamHandler(feedWatchProcedure, fn))
	srv := httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func connectExec(url string, opts Options) *HTTPExecutor {
	opts.StreamProtocol = StreamProtocolConnect
	return NewHTTPExecutor(url, nil, Credential{}, opts)
}

// TestConnectStreamRealServer: three messages from a real Connect server arrive
// in order, the request message reached the handler intact, and a clean
// end-of-stream reads as a normal end.
func TestConnectStreamRealServer(t *testing.T) {
	var gotTopic string
	srv := newConnectFeedServer(t, func(_ context.Context, req *connect.Request[structpb.Struct], st *connect.ServerStream[structpb.Struct]) error {
		gotTopic = req.Msg.GetFields()["topic"].GetStringValue()
		for i := 1; i <= 3; i++ {
			msg, _ := structpb.NewStruct(map[string]any{"text": fmt.Sprintf("msg-%d", i)})
			if err := st.Send(msg); err != nil {
				return err
			}
		}
		return nil
	})

	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()
	id, err := connectExec(srv.URL, Options{}).StartServerStream(m, watchOp(), map[string]any{"topic": "news"})
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	frames, res := waitFrames(t, m, id, 3, true)
	if len(frames) != 3 || !strings.Contains(frames[0], "msg-1") || !strings.Contains(frames[2], "msg-3") {
		t.Fatalf("frames = %v, want the 3 Connect messages in order", frames)
	}
	if !res.Done || res.Reason != "stream ended" {
		t.Fatalf("end state = %+v, want a clean 'stream ended'", res)
	}
	if gotTopic != "news" {
		t.Errorf("handler saw topic %q, want the enveloped request message", gotTopic)
	}
}

// TestConnectStreamEndStreamError: a Connect error returned by the handler
// after one message travels in the end-of-stream envelope; its code and message
// become the session's end reason, and the frame before it stays pollable.
func TestConnectStreamEndStreamError(t *testing.T) {
	srv := newConnectFeedServer(t, func(_ context.Context, _ *connect.Request[structpb.Struct], st *connect.ServerStream[structpb.Struct]) error {
		msg, _ := structpb.NewStruct(map[string]any{"text": "ok-1"})
		if err := st.Send(msg); err != nil {
			return err
		}
		return connect.NewError(connect.CodeResourceExhausted, errors.New("quota gone"))
	})

	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()
	id, err := connectExec(srv.URL, Options{}).StartServerStream(m, watchOp(), nil)
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	frames, res := waitFrames(t, m, id, 1, true)
	if len(frames) != 1 || !strings.Contains(frames[0], "ok-1") {
		t.Fatalf("frames = %v, want the message sent before the error", frames)
	}
	if !res.Done || !strings.Contains(res.Reason, "resource_exhausted") || !strings.Contains(res.Reason, "quota gone") {
		t.Fatalf("end reason = %q, want the Connect error code and message", res.Reason)
	}
}

// TestConnectStreamRedaction: enveloped frames AND the end-stream error pass
// through the redactor before reaching the ring, exactly like NDJSON frames.
func TestConnectStreamRedaction(t *testing.T) {
	srv := newConnectFeedServer(t, func(_ context.Context, _ *connect.Request[structpb.Struct], st *connect.ServerStream[structpb.Struct]) error {
		msg, _ := structpb.NewStruct(map[string]any{"token": "sekrit-value"})
		_ = st.Send(msg)
		return connect.NewError(connect.CodeInternal, errors.New("leaked sekrit-value"))
	})
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()
	exec := connectExec(srv.URL, Options{
		Redact: func(s string) string { return strings.ReplaceAll(s, "sekrit-value", "[REDACTED]") },
	})
	id, err := exec.StartServerStream(m, watchOp(), nil)
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	frames, res := waitFrames(t, m, id, 1, true)
	if strings.Contains(frames[0], "sekrit-value") || !strings.Contains(frames[0], "[REDACTED]") {
		t.Fatalf("frame not redacted: %q", frames[0])
	}
	if strings.Contains(res.Reason, "sekrit-value") {
		t.Fatalf("end-stream error not redacted: %q", res.Reason)
	}
}

// envelope builds one Connect frame: flag byte, 4-byte big-endian length, payload.
func envelope(flag byte, payload []byte) []byte {
	b := make([]byte, 5, 5+len(payload))
	b[0] = flag
	binary.BigEndian.PutUint32(b[1:], uint32(len(payload)))
	return append(b, payload...)
}

// rawConnectServer answers every request as a Connect stream with exactly the
// given bytes, after recording what the client sent.
func rawConnectServer(t *testing.T, body []byte, seen *http.Request, seenBody *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = *r.Clone(context.Background())
		}
		if seenBody != nil {
			*seenBody, _ = io.ReadAll(r.Body)
		}
		w.Header().Set("Content-Type", "application/connect+json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runRaw starts a connect-mode stream against a raw server and waits for it to end.
func runRaw(t *testing.T, body []byte, opts Options, wantFrames int) ([]string, *PollResult) {
	t.Helper()
	srv := rawConnectServer(t, body, nil, nil)
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	t.Cleanup(m.CloseAll)
	id, err := connectExec(srv.URL, opts).StartServerStream(m, watchOp(), nil)
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	return waitFrames(t, m, id, wantFrames, true)
}

// TestConnectStreamRequestIsEnveloped: the request is ONE Connect envelope
// (flag 0, exact length) carrying the JSON message, with the Connect streaming
// content type and protocol version, and no compression offered.
func TestConnectStreamRequestIsEnveloped(t *testing.T) {
	var req http.Request
	var body []byte
	srv := rawConnectServer(t, envelope(0x02, []byte(`{}`)), &req, &body)
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()
	id, err := connectExec(srv.URL, Options{}).StartServerStream(m, watchOp(), map[string]any{"topic": "news"})
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	waitFrames(t, m, id, 0, true)

	if ct := req.Header.Get("Content-Type"); ct != "application/connect+json" {
		t.Errorf("Content-Type = %q, want application/connect+json", ct)
	}
	if v := req.Header.Get("Connect-Protocol-Version"); v != "1" {
		t.Errorf("Connect-Protocol-Version = %q, want 1", v)
	}
	if enc := req.Header.Get("Connect-Content-Encoding"); enc != "" && enc != "identity" {
		t.Errorf("Connect-Content-Encoding = %q, want none/identity", enc)
	}
	if enc := req.Header.Get("Connect-Accept-Encoding"); enc != "" && enc != "identity" {
		t.Errorf("Connect-Accept-Encoding = %q: we must not offer compression we cannot decode", enc)
	}
	if len(body) < 5 || body[0] != 0 {
		t.Fatalf("request body %q is not an uncompressed Connect envelope", body)
	}
	n := binary.BigEndian.Uint32(body[1:5])
	if int(n) != len(body)-5 || string(body[5:]) != `{"topic":"news"}` {
		t.Fatalf("envelope length %d / payload %q, want the exact JSON request message", n, body[5:])
	}
}

// TestConnectStreamMalformed: every way the framing can be wrong ends the
// session with an error reason that names the problem — never a hang, never a
// silent clean end, never garbage frames.
func TestConnectStreamMalformed(t *testing.T) {
	ok := envelope(0x00, []byte(`{"text":"first"}`))
	cases := []struct {
		name       string
		body       []byte
		opts       Options
		wantFrames int
		wantReason string
	}{
		{"truncated header", append(append([]byte{}, ok...), 0x00, 0x00), Options{}, 1, "truncated"},
		{"truncated payload", append(append([]byte{}, ok...), envelope(0x00, []byte(`{"text":"second"}`))[:9]...), Options{}, 1, "truncated"},
		{"missing end-of-stream", ok, Options{}, 1, "end-of-stream"},
		{"compressed flag", append(append([]byte{}, ok...), envelope(0x01, []byte("gzip-bytes"))...), Options{}, 1, "compressed"},
		{"unknown flag bits", envelope(0x80, []byte(`{}`)), Options{}, 0, "flag"},
		{"bad end-stream JSON", envelope(0x02, []byte(`{not json`)), Options{}, 0, "end-of-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames, res := runRaw(t, tc.body, tc.opts, tc.wantFrames)
			if len(frames) != tc.wantFrames {
				t.Fatalf("frames = %v, want %d", frames, tc.wantFrames)
			}
			if !res.Done || res.Reason == "stream ended" || !strings.Contains(res.Reason, tc.wantReason) {
				t.Fatalf("end reason = %q, want an error mentioning %q", res.Reason, tc.wantReason)
			}
		})
	}
}

// TestConnectStreamOversizeRefusedBeforeAlloc: a length prefix above MaxBody is
// refused from the header alone. The server announces 1 GiB and sends nothing
// more; if the client trusted the prefix it would either allocate it or wait
// for bytes that never come — instead it must fail fast naming the cap.
func TestConnectStreamOversizeRefusedBeforeAlloc(t *testing.T) {
	hdr := make([]byte, 5)
	binary.BigEndian.PutUint32(hdr[1:], 1<<30)
	held := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+json")
		_, _ = w.Write(hdr)
		w.(http.Flusher).Flush()
		select { // keep the connection open: a client reading the payload would hang
		case <-r.Context().Done():
		case <-held:
		}
	}))
	defer srv.Close()
	defer close(held)

	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()
	id, err := connectExec(srv.URL, Options{MaxBody: 1024}).StartServerStream(m, watchOp(), nil)
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	_, res := waitFrames(t, m, id, 0, true)
	if !strings.Contains(res.Reason, "1024") || !strings.Contains(res.Reason, "cap") {
		t.Fatalf("end reason = %q, want a refusal naming the 1024-byte cap", res.Reason)
	}
}

// TestConnectModeRejectsNonConnectResponse: opted into Connect, a server that
// answers with plain JSON/NDJSON is a misconfiguration, reported as such rather
// than decoded as envelopes.
func TestConnectModeRejectsNonConnectResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"result":{"text":"ndjson"}}`)
	}))
	defer srv.Close()
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()
	id, err := connectExec(srv.URL, Options{}).StartServerStream(m, watchOp(), nil)
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	frames, res := waitFrames(t, m, id, 0, true)
	if len(frames) != 0 || !strings.Contains(res.Reason, "application/json") || !strings.Contains(res.Reason, "connect") {
		t.Fatalf("frames=%v reason=%q, want a content-type mismatch naming both sides", frames, res.Reason)
	}
}
