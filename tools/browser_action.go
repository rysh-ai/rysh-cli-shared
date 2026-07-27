package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	natsclient "github.com/nats-io/nats.go"
)

// BrowserActionPublisher is the interface the BrowserActionTool needs to send
// requests. This avoids importing the msg package directly (which causes
// cross-module import path conflicts with the replace directive).
type BrowserActionPublisher interface {
	// SendBrowserAction publishes a browser action request envelope to the given
	// NATS subject. The implementation is responsible for NATSEnvelope encoding.
	SendBrowserAction(subject string, requestID, action string, params json.RawMessage) error
}

// BrowserActionTool allows the LLM to control the user's Chrome browser
// through the Rysh Chrome extension. Actions include navigating to URLs,
// clicking elements, filling forms, taking screenshots, and extracting
// page content.
//
// The tool publishes a browser action request to the pane's NATS subject and
// waits for a response from the extension.
type BrowserActionTool struct {
	paneID      string
	nc          *natsclient.Conn
	pub         BrowserActionPublisher
	reqSubject  string // e.g. "rysh.pane.{id}.browser.request"
	respSubject string // e.g. "rysh.pane.{id}.browser.response"
}

// NewBrowserActionTool creates a BrowserActionTool for the given pane.
// subjectFn builds NATS subjects from parts (e.g. msg.T).
func NewBrowserActionTool(paneID string, nc *natsclient.Conn, pub BrowserActionPublisher, reqSubject, respSubject string) *BrowserActionTool {
	return &BrowserActionTool{
		paneID:      paneID,
		nc:          nc,
		pub:         pub,
		reqSubject:  reqSubject,
		respSubject: respSubject,
	}
}

// browserActionSpec is the JSON Schema for the browser_action tool parameters.
var browserActionSpec = json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "description": "The browser action to execute. One of: navigate, click, type, select, check, scroll, hover, wait, screenshot, get_text, get_html, get_elements, get_value, get_tabs, switch_tab, new_tab, close_tab, back, forward, reload, execute_js, press_key, paste, drag_drop"
    },
    "params": {
      "type": "object",
      "description": "Action-specific parameters. See the tool description for each action's params."
    }
  },
  "required": ["action"]
}`)

// Spec returns the tool specification for the LLM.
func (t *BrowserActionTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "browser_action",
		Description: `Execute a browser action in the user's Chrome browser via the Rysh Chrome extension.

Available actions and their params:

Navigation:
- navigate: Go to a URL. Params: {"url": "https://example.com"}
- back: Go back in history. Params: {}
- forward: Go forward in history. Params: {}
- reload: Reload the page. Params: {}

Element interaction:
- click: Click an element. Params: {"selector": "CSS or special selector", "text": "optional filter text", "index": 0}
- type: Type text into a field. Params: {"selector": "input selector", "text": "text to type", "clear": true, "keystrokes": false}. Text is delivered as TRUSTED browser-level input. Default is fast bulk insertion; set "keystrokes": true to send one real key event per character — REQUIRED for fields that only react to actual keydowns (e.g. editor default/placeholder text that must clear on the first keystroke, or markdown-style shortcuts).
- select: Select dropdown option. Params: {"selector": "select selector", "value": "option value", "text": "option text"}
- check: Check/uncheck a checkbox. Params: {"selector": "checkbox selector", "checked": true}
- hover: Hover over an element. Params: {"selector": "element selector"}
- press_key: Send one TRUSTED keyboard press. Params: {"key": "Enter", "modifiers": ["meta","alt"], "count": 1}. Modifiers are pressed as real physical keys around the main key, so selection (Shift+Arrow/Home/End) and shortcuts (Cmd+A, Cmd+Z, Cmd+Alt+1) work. Use "count" (max 200) for legitimate repeats (e.g. Backspace x 12) instead of re-sending identical calls.
- paste: Native rich paste at the caret (desktop-app embedded panes only). Params: {"html": "<h1>...</h1><p>...</p>"} seeds the clipboard with HTML first (the editor runs its own rich-paste conversion - by far the FASTEST way to insert long formatted content), {"text": "..."} for plain text, {} pastes the current system clipboard. Synthetic Cmd+V never works; always use this action to paste.
- drag_drop: Drag and drop. Params: {"from_selector": "source", "to_selector": "target"}

Scrolling:
- scroll: Scroll the page. Params: {"direction": "down", "amount": 500, "selector": "optional container"}

Waiting:
- wait: Wait for element or page load. Params: {"selector": "CSS selector", "timeout_ms": 10000, "visible": true}

Content extraction:
- get_text: Get text content. Params: {"selector": "optional CSS selector"} (omit for full page)
- get_html: Get HTML content. Params: {"selector": "optional CSS selector", "outer": false}
- get_elements: Query elements. Params: {"selector": "CSS selector", "attributes": ["href", "src"], "limit": 50}
- get_value: Get input field value. Params: {"selector": "input selector"}

Screenshot:
- screenshot: Capture the visible tab. Params: {}

Tab management:
- get_tabs: List open browser tabs. Params: {}
- switch_tab: Switch to a tab. Params: {"tab_id": 123} or {"index": 0} or {"url_pattern": "*github*"}
- new_tab: Open a new tab. Params: {"url": "https://example.com"}
- close_tab: Close a tab. Params: {"tab_id": 123} (omit for active tab)

JavaScript execution (requires approval):
- execute_js: Run arbitrary JavaScript. Params: {"code": "document.title"}

Selector formats (for all element-targeting actions):
- CSS selector: "#login-button", ".submit-btn", "input[name='email']"
- XPath: "xpath://button[contains(text(),'Submit')]" or "//button[text()='OK']"
- Text match: "text:Sign In" (finds element containing text)
- ARIA label: "aria:Submit button"
- Role: "role:button"
- Test ID: "testid:login-btn" (matches data-testid)`,
		Parameters:       browserActionSpec,
		RequiresApproval: false, // dynamic — see RequiresApproval method
	}
}

// RequiresApproval returns true only for execute_js which runs arbitrary code.
func (t *BrowserActionTool) RequiresApproval(params json.RawMessage) bool {
	var p struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return false
	}
	return p.Action == "execute_js"
}

// browserActionResponse is the decoded response from the Chrome extension.
// Matches MsgBrowserActionResponse but defined locally to avoid msg import.
type browserActionResponse struct {
	RequestID  string          `json:"request_id"`
	Success    bool            `json:"success"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	Screenshot string          `json:"screenshot,omitempty"`
}

// natsEnvelope mirrors msg.NATSEnvelope for decoding responses. Payload is kept
// as raw JSON: the standard NATSPublisher inlines it as a JSON object, while some
// publishers (the Chrome extension's NATSClient.encode) send it as a base64 JSON
// string — decodeBrowserActionResponse handles both forms.
type natsEnvelope struct {
	TypeTag string          `json:"t"`
	ReplyTo string          `json:"r"`
	Payload json.RawMessage `json:"p"`
}

// Execute sends a browser action request to the Chrome extension via NATS
// and waits for the response.
func (t *BrowserActionTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var input struct {
		Action string          `json:"action"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(params, &input); err != nil {
		return ErrOutput(ErrKindValidation, "invalid params: "+err.Error()), nil
	}
	if input.Action == "" {
		return ErrOutput(ErrKindValidation, "missing required parameter: action"), nil
	}
	if input.Params == nil {
		input.Params = json.RawMessage(`{}`)
	}

	requestID := uuid.New().String()

	// Subscribe to response subject before publishing the request.
	sub, err := t.nc.SubscribeSync(t.respSubject)
	if err != nil {
		return ErrOutput(ErrKindInternal, "failed to subscribe for browser response: "+err.Error()), nil
	}
	defer func() { _ = sub.Unsubscribe() }()

	// Publish request to the Chrome extension.
	if err := t.pub.SendBrowserAction(t.reqSubject, requestID, input.Action, input.Params); err != nil {
		return ErrOutput(ErrKindTransient, "failed to send browser action request: "+err.Error()), nil
	}

	// Wait for matching response (60s timeout).
	timeout := 60 * time.Second
	deadline := time.After(timeout)
	for {
		select {
		case <-ctx.Done():
			return ErrOutput(ErrKindCancelled, "browser action cancelled"), nil
		case <-deadline:
			return ErrOutputf(ErrKindTimeout, "browser action %q timed out after %s — the Chrome extension may not be connected", input.Action, timeout), nil
		default:
			natsMsg, err := sub.NextMsg(500 * time.Millisecond)
			if err != nil {
				continue // NextMsg timeout, try again
			}

			// Decode the NATSEnvelope to get the response.
			resp, err := decodeBrowserActionResponse(natsMsg.Data)
			if err != nil {
				continue // not a valid response, skip
			}
			if resp.RequestID != requestID {
				continue // not our response
			}

			if !resp.Success {
				return ErrOutput(ErrKindInternal, resp.Error), nil
			}

			content := shapeScreenshotContent(input.Action, string(resp.Result), resp.Screenshot)
			out := &ToolOutput{Content: content}
			if resp.Screenshot != "" {
				if out.Metadata == nil {
					out.Metadata = make(map[string]string)
				}
				out.Metadata["screenshot_base64"] = resp.Screenshot
			}
			return out, nil
		}
	}
}

// decodeBrowserActionResponse decodes a NATSEnvelope containing a browser action response.
func decodeBrowserActionResponse(data []byte) (*browserActionResponse, error) {
	var env natsEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if env.TypeTag != "MsgBrowserActionResponse" {
		return nil, fmt.Errorf("unexpected tag: %s", env.TypeTag)
	}
	// The standard NATSPublisher inlines the payload as a JSON object; some
	// publishers encode it as a base64 (or plain) JSON string. Handle both so
	// the response round-trip works regardless of which side published it.
	payload := []byte(env.Payload)
	if len(payload) > 0 && payload[0] == '"' {
		var s string
		if err := json.Unmarshal(payload, &s); err == nil {
			if decoded, derr := base64.StdEncoding.DecodeString(s); derr == nil {
				payload = decoded
			} else {
				payload = []byte(s)
			}
		}
	}
	var resp browserActionResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &resp, nil
}

// shapeScreenshotContent makes a screenshot action's tool_result text honest
// about whether pixels were actually captured. Executors reply with a bare
// {"screenshot":"captured"} regardless; when the image payload is EMPTY (a
// boot race, a blank page, or a failed capture) that text misleads the model
// into believing it observed the page — the exact hallucination window a
// live test exposed. Non-screenshot actions pass through untouched.
func shapeScreenshotContent(action, content, screenshot string) string {
	if action != "screenshot" {
		return content
	}
	if screenshot == "" {
		return `{"screenshot":"empty"} — NO image was returned (the page may still be loading, ` +
			`or the capture failed). Do NOT assume anything about the page content from this ` +
			`result; wait for the page to load and take the screenshot again.`
	}
	return content + `
The captured image follows this tool result as an attached user image — observe it.`
}
