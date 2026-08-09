// Package mcpclient is a minimal Model Context Protocol (MCP) client for
// server-side rysh components.
//
// It exists because the chatbot's LLM pane runs inside rysh-server, which had
// no way to reach an MCP server: the complete client under
// rysh-cli/internal/mcp lives in a module deliberately outside go.work and is
// package-private besides, so rysh-server cannot import it. This package is the
// server-side half — the wire shapes are kept identical to the CLI's so the two
// cannot drift into speaking different dialects of the same protocol.
//
// Scope, deliberately small: the Streamable-HTTP transport and the three
// methods a tool consumer needs — initialize, tools/list, tools/call. The CLI's
// process lifecycle, heartbeats, auto-restart and stdio transport are not
// reproduced here; a hosted customer MCP server speaks HTTP, and a server-side
// process supervisor is a different problem than the one this solves.
//
// Discovered tools are adapted to tools.ToolExecutor (see executor.go) so they
// register into the same registry as native rysh tools and flow through the
// existing tool-use bridge, approval gate, and audit path unchanged.
package mcpclient

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the MCP revision this client advertises during the
// initialize handshake. It matches rysh-cli/internal/mcp and the reference
// servers under rysh-mcp-samples.
const ProtocolVersion = "2024-11-05"

// notificationInitialized is sent by the client after a successful initialize.
// Servers tolerate its absence but expect it.
const notificationInitialized = "notifications/initialized"

// jsonrpcRequest is an outgoing JSON-RPC 2.0 request or notification. An empty
// ID marks the message as a notification (no response is expected).
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonrpcMessage is a permissive inbound envelope: a response (id +
// result/error) or a notification (method, no id).
type jsonrpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object. It implements error.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "<nil rpc error>"
	}
	if len(e.Data) > 0 {
		return fmt.Sprintf("rpc error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// implementation identifies a client or server in the initialize handshake.
type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// initializeParams is the body of the "initialize" request.
type initializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    clientCapabilities `json:"capabilities"`
	ClientInfo      implementation     `json:"clientInfo"`
}

// clientCapabilities advertises optional client features. This client consumes
// tools only, so it is intentionally empty.
type clientCapabilities struct{}

// initializeResult is the server's reply to "initialize".
type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      implementation     `json:"serverInfo"`
}

type serverCapabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type toolsCapability struct {
	// ListChanged indicates the server will emit
	// notifications/tools/list_changed. This client does not hold a
	// server→client stream, so it is recorded but not acted on.
	ListChanged bool `json:"listChanged"`
}

// Tool is a tool advertised by an MCP server. InputSchema stays raw JSON so it
// can be handed to tools.ToolSpec.Parameters (also a JSON Schema) without a
// lossy round-trip through a typed struct.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// toolsListParams carries the optional pagination cursor for "tools/list".
type toolsListParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// toolsListResult is the server's reply to "tools/list". A non-empty NextCursor
// means more tools are available.
type toolsListResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// callToolParams is the body of the "tools/call" request.
type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallToolResult is the server's reply to "tools/call". IsError distinguishes
// "the tool ran and reported a failure" (recoverable by the model) from a
// transport or protocol failure (returned as a Go error).
type CallToolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// contentBlock is one element of a tool result. Text is rendered natively;
// non-text blocks are summarized rather than dropped silently.
type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Data     string          `json:"data,omitempty"`
	MIMEType string          `json:"mimeType,omitempty"`
	Resource json.RawMessage `json:"resource,omitempty"`
}
