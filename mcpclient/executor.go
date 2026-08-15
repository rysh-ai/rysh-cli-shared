// SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// Executors adapts listed MCP tools to the shared ToolExecutor interface, so
// they register into the same ToolRegistry as native rysh tools and flow
// through the existing tool-use bridge, approval gate, and audit path.
func (c *Client) Executors(list []Tool) []sharedtools.ToolExecutor {
	out := make([]sharedtools.ToolExecutor, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, t := range list {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		name := registryName(c.cfg.Name, t.Name)
		if seen[name] {
			// Two remote names that sanitize to the same registry name would
			// otherwise shadow each other unpredictably. Keep the first.
			continue
		}
		seen[name] = true

		schema := t.InputSchema
		if len(strings.TrimSpace(string(schema))) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		approval := c.cfg.Approval == ApprovalAlways
		out = append(out, &toolExecutor{
			client:     c,
			serverName: c.cfg.Name,
			remoteName: t.Name,
			approval:   approval,
			spec: sharedtools.ToolSpec{
				Name:             name,
				Description:      t.Description,
				Parameters:       schema,
				RequiresApproval: approval,
			},
		})
	}
	return out
}

// registryName prefixes a remote tool name with the server alias and coerces
// the result into the character set the tool API accepts.
func registryName(server, tool string) string {
	if strings.TrimSpace(server) == "" {
		return sanitizeToolName(tool)
	}
	return sanitizeToolName(server + "_" + tool)
}

// sanitizeToolName coerces a name into ^[a-zA-Z0-9_-]{1,64}$: invalid runes
// become '_', the result is truncated to 64 bytes, and an empty result falls
// back to "tool". Same rule as rysh-cli/internal/mcp so a tool keeps the same
// registry name whichever side of the product adopts it.
func sanitizeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" {
		s = "tool"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// toolExecutor adapts one MCP tool to tools.ToolExecutor.
type toolExecutor struct {
	client     *Client
	serverName string // MCP server alias (diagnostics + tool-name prefix)
	remoteName string // tool name as the server knows it (used in tools/call)
	spec       sharedtools.ToolSpec
	approval   bool
}

// Spec returns the registry-facing definition (prefixed name + JSON Schema).
func (e *toolExecutor) Spec() sharedtools.ToolSpec { return e.spec }

// RequiresApproval reflects the server's configured ApprovalPolicy — true
// unless the operator explicitly vetted the server. It ignores the call's
// params: nothing in an MCP tool call distinguishes a safe argument set from a
// dangerous one without understanding the remote tool's semantics, which this
// client by construction does not. See ApprovalAlways for the full reasoning.
func (e *toolExecutor) RequiresApproval(json.RawMessage) bool { return e.approval }

// Execute forwards the call to the MCP server and renders the result.
//
// Every failure comes back as a *ToolOutput with a typed ErrorKind rather than
// a Go error, so the model reads the failure and can recover (retry with
// corrected arguments, or fall back to answering without the tool) instead of
// the turn dying. The kinds mirror the taxonomy in tools/registry.go.
func (e *toolExecutor) Execute(ctx context.Context, params json.RawMessage) (*sharedtools.ToolOutput, error) {
	res, err := e.client.CallTool(ctx, e.remoteName, params)
	if err != nil {
		kind, msg := classify(err)
		return sharedtools.ErrOutputf(kind, "mcp %s/%s: %s", e.serverName, e.remoteName, msg), nil
	}
	text := renderContent(res.Content)
	if res.IsError {
		// The tool ran and reported failure. That is the tool's own output, not
		// a transport problem, so it is handed to the model verbatim.
		if text == "" {
			text = "tool reported an error"
		}
		return &sharedtools.ToolOutput{Error: text, ErrorKind: sharedtools.ErrKindValidation}, nil
	}
	return &sharedtools.ToolOutput{
		Content:  text,
		Metadata: map[string]string{"mcp_server": e.serverName, "mcp_tool": e.remoteName},
	}, nil
}

// classify maps a transport/protocol failure to a ToolOutput error kind.
func classify(err error) (kind, msg string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return sharedtools.ErrKindTimeout, "timed out"
	case errors.Is(err, context.Canceled):
		return sharedtools.ErrKindCancelled, "cancelled"
	case isGuardErr(err):
		return sharedtools.ErrKindPermissionDenied, err.Error()
	}
	var se *sizeError
	if errors.As(err, &se) {
		return sharedtools.ErrKindInternal, se.Error()
	}
	var re *rpcError
	if errors.As(err, &re) {
		switch re.Code {
		case -32600, -32602:
			// Invalid request / invalid params: the model can fix its arguments.
			return sharedtools.ErrKindValidation, re.Error()
		case -32601:
			// Method (or tool) not found — the catalog and the server disagree.
			return sharedtools.ErrKindMissing, re.Error()
		default:
			return sharedtools.ErrKindTransient, re.Error()
		}
	}
	return sharedtools.ErrKindTransient, err.Error()
}

// renderContent flattens MCP content blocks into a single string. Text is used
// verbatim; non-text blocks are summarized rather than dropped silently.
func renderContent(blocks []contentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		switch {
		case b.Type == "text" || (b.Text != "" && b.Type == ""):
			sb.WriteString(b.Text)
		case b.Type == "image":
			fmt.Fprintf(&sb, "[image content: %s, %d bytes base64]", orUnknown(b.MIMEType), len(b.Data))
		case b.Type == "audio":
			fmt.Fprintf(&sb, "[audio content: %s, %d bytes base64]", orUnknown(b.MIMEType), len(b.Data))
		case b.Type == "resource" || len(b.Resource) > 0:
			fmt.Fprintf(&sb, "[resource: %s]", strings.TrimSpace(string(b.Resource)))
		case b.Text != "":
			sb.WriteString(b.Text)
		default:
			fmt.Fprintf(&sb, "[%s content]", orUnknown(b.Type))
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
