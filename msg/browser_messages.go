// SPDX-License-Identifier: Apache-2.0

package msg

import "encoding/json"

// ---------------------------------------------------------------------------
// Browser action message tags
// ---------------------------------------------------------------------------

const (
	TagBrowserActionRequest  = "MsgBrowserActionRequest"
	TagBrowserActionResponse = "MsgBrowserActionResponse"
)

// ---------------------------------------------------------------------------
// Browser Action Messages
// ---------------------------------------------------------------------------

// MsgBrowserActionRequest is sent from the server-side orchestrator to the
// Chrome extension requesting a browser action (navigate, click, type, etc.).
// The extension executes the action and replies with MsgBrowserActionResponse.
type MsgBrowserActionRequest struct {
	RequestID string          `json:"request_id"` // UUID for correlation
	Action    string          `json:"action"`     // Action name: navigate, click, type, etc.
	Params    json.RawMessage `json:"params"`     // Action-specific parameters
}

// MsgBrowserActionResponse is sent from the Chrome extension back to the
// server after executing a browser action.
type MsgBrowserActionResponse struct {
	RequestID  string          `json:"request_id"`           // Echoes request ID
	Success    bool            `json:"success"`              // Whether the action succeeded
	Result     json.RawMessage `json:"result,omitempty"`     // Action-specific result data
	Error      string          `json:"error,omitempty"`      // Error message if !success
	Screenshot string          `json:"screenshot,omitempty"` // Optional base64 PNG
}

// RegisterBrowserCodecs registers all browser action message types with the
// given codec registry.
func RegisterBrowserCodecs(r *CodecRegistry) {
	r.Register(TagBrowserActionRequest, "*msg.MsgBrowserActionRequest", jsonDecoder[MsgBrowserActionRequest]())
	r.Register(TagBrowserActionResponse, "*msg.MsgBrowserActionResponse", jsonDecoder[MsgBrowserActionResponse]())
}
