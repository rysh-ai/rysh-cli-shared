// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// Connect streaming protocol, server-streaming half, JSON codec only (design
// 015 §5.3; https://connectrpc.com/docs/protocol#streaming-rpcs).
//
// Every message in both directions is an envelope: one flag byte, a 4-byte
// big-endian payload length, then the payload. Flag 0x01 marks a compressed
// payload, 0x02 the end-of-stream message — a JSON object
// {"error":{"code","message","details"}?, "metadata":{…}?} that closes the
// response. Compression is never negotiated (we send no
// Connect-Accept-Encoding), so a compressed frame is a server fault and is
// refused rather than guessed at. Client-streaming and bidi are out of scope by
// ruling (015 §5.1): the request side is always exactly one envelope.

const (
	connectJSONContentType = "application/connect+json"

	connectFlagCompressed = 0x01
	connectFlagEndStream  = 0x02
)

// connectEnvelope wraps one uncompressed message payload in a Connect envelope.
func connectEnvelope(payload []byte) []byte {
	b := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(b[1:], uint32(len(payload)))
	return append(b, payload...)
}

// connectDispatch decides, from the response headers, whether the body is
// Connect envelopes. It is keyed on what the server SAID it sent, not on what
// was asked for: a Connect-labelled body is decoded as Connect even on a
// gateway-mode call, a Connect body in a codec we do not speak is refused, and
// a Connect-mode call answered with anything else is reported as the
// misconfiguration it is instead of being parsed as NDJSON.
func connectDispatch(h http.Header, connectMode bool) (bool, error) {
	raw := h.Get("Content-Type")
	ct, _, err := mime.ParseMediaType(raw)
	if err != nil {
		ct = strings.ToLower(strings.TrimSpace(raw))
	}
	switch {
	case ct == connectJSONContentType:
		if enc := h.Get("Connect-Content-Encoding"); enc != "" && enc != "identity" {
			return false, fmt.Errorf("Connect stream uses %q compression, which forge does not negotiate", enc)
		}
		return true, nil
	case strings.HasPrefix(ct, "application/connect+"):
		return false, fmt.Errorf("server answered %s; forge speaks only the Connect JSON codec (%s)", ct, connectJSONContentType)
	case connectMode:
		return false, fmt.Errorf("API is set to --grpc-protocol connect but the server answered Content-Type %q, not %s (a grpc-gateway bridge wants --grpc-protocol gateway)", raw, connectJSONContentType)
	}
	return false, nil
}

// consumeConnect reads Connect envelopes until the end-of-stream message,
// pushing each message (redacted, pretty-printed). Every way the framing can be
// wrong ends the stream with an error naming it; a length prefix above
// Options.MaxBody is refused from the header alone, before allocating or
// reading the payload.
func (e *HTTPExecutor) consumeConnect(r io.Reader, push func(string)) error {
	var hdr [5]byte
	for {
		n, err := io.ReadFull(r, hdr[:])
		switch {
		case errors.Is(err, io.EOF):
			return fmt.Errorf("Connect stream closed without an end-of-stream message")
		case errors.Is(err, io.ErrUnexpectedEOF):
			return fmt.Errorf("truncated Connect envelope header (%d of 5 bytes)", n)
		case err != nil:
			return err
		}
		flags := hdr[0]
		size := binary.BigEndian.Uint32(hdr[1:])
		if flags&connectFlagCompressed != 0 {
			return fmt.Errorf("compressed Connect frame refused: forge does not negotiate Connect compression")
		}
		if flags&^connectFlagEndStream != 0 {
			return fmt.Errorf("Connect envelope has unknown flag bits 0x%02x", flags)
		}
		if int64(size) > e.opts.MaxBody {
			return fmt.Errorf("Connect frame of %d bytes exceeded the %d-byte cap", size, e.opts.MaxBody)
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("truncated Connect envelope payload (want %d bytes)", size)
			}
			return err
		}
		if flags&connectFlagEndStream != 0 {
			return e.connectEndStream(payload)
		}
		push(e.redact(prettyJSON(payload)))
	}
}

// connectEndStream interprets the end-of-stream message: nil for a clean end,
// the Connect error (code: message, redacted) otherwise. Trailing metadata is
// not surfaced.
func (e *HTTPExecutor) connectEndStream(payload []byte) error {
	var end struct {
		Error *struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &end); err != nil {
		return fmt.Errorf("malformed Connect end-of-stream message: %v", err)
	}
	if end.Error == nil {
		return nil
	}
	code := end.Error.Code
	if code == "" {
		code = "unknown"
	}
	return fmt.Errorf("stream error frame: %s", truncate(e.redact(code+": "+end.Error.Message), 1024))
}

// connectHint names the fix when a gateway-mode gRPC call is refused the way a
// Connect server refuses non-Connect requests: HTTP 415 (a streaming procedure
// sent plain JSON) or a demand for the Connect-Protocol-Version header (a
// strict server). Empty in connect mode or for any other failure.
func (e *HTTPExecutor) connectHint(status int, body string) string {
	if e.opts.StreamProtocol == StreamProtocolConnect {
		return ""
	}
	if status == http.StatusUnsupportedMediaType || strings.Contains(body, "Connect-Protocol-Version") {
		return " (if this is a Connect server, re-add the API with --grpc-protocol connect)"
	}
	return ""
}
