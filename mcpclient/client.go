package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// clientVersion is advertised to servers in the initialize handshake.
const clientVersion = "0.1.0"

// Hardening defaults. They sit in the same band as the sibling server-side
// tools that also make outbound calls on a model's behalf: browse_web uses
// 2 MB / 10 s, the owner-configured HTTP tool caps bodies at 256 KB.
const (
	// DefaultTimeout bounds a single JSON-RPC round trip. A tool call that
	// queries a customer's live business data is slower than a page fetch, so
	// this is looser than browse_web's 10 s — but it is still a hard bound, and
	// the pane's own context cancels earlier when the visitor moves on.
	DefaultTimeout = 15 * time.Second

	// DefaultMaxResponseBytes caps one HTTP response body. Larger than the HTTP
	// tool's 256 KB because a tools/list catalog is structural rather than
	// model-facing prose, small enough that a hostile server cannot exhaust
	// server memory through a tool the model called on its own initiative.
	DefaultMaxResponseBytes = 1 << 20

	// DefaultMaxTools bounds how many tools one server may contribute. Every
	// adopted tool costs prompt tokens on every turn, so an unbounded catalog
	// is a cost and a context-window problem before it is a security one.
	DefaultMaxTools = 64

	// maxToolPages bounds tools/list pagination independently of MaxTools, so a
	// server that always returns a nextCursor cannot spin the client forever.
	maxToolPages = 100
)

// URLGuard authorizes one outbound request target. It is applied to the
// configured endpoint before every request and again to every redirect hop.
//
// ---------------------------------------------------------------------------
// Which layer owns SSRF protection: the caller, not this package.
// ---------------------------------------------------------------------------
//
// This package owns *transport* hardening — timeout, response-size cap, tool
// count, redirect bound. It deliberately does not own *network-position*
// policy (http(s)-only is checked here as basic input validation, but "is this
// host publicly routable" is not), for one reason: rysh-server already has that
// policy, implemented and tested, in internal/agentic/chatbot_ssrf_guard.go,
// and its own header says a second copy is "a future vulnerability, not a
// convenience". Duplicating it in rysh-shared would create exactly the drift it
// warns about — and rysh-shared has no visibility into a deployment's egress
// rules anyway.
//
// So the policy is injected, and Guard is REQUIRED: New refuses a nil Guard
// rather than defaulting to allow-all. That refusal is the whole point. The
// custom HTTP tool shipped with a `strings.HasPrefix(url, "http")` check and
// nothing else (design 016 gap 3), which let an owner aim a tool at the cloud
// metadata endpoint. A library default of "no guard means no guarding" is how
// that happens again; a compile-time-visible required field is how it does not.
//
// Wave 2 (wiring MCP into chatbot_configs) must pass rysh-server's
// strictURLGuard here. Tests against a loopback httptest server pass
// AllowAnyURL.
type URLGuard func(ctx context.Context, u *url.URL) error

// AllowAnyURL is an explicit, deliberately loud escape hatch that authorizes
// every target. It is for tests binding to loopback and for callers that have
// already applied their own egress policy at a lower layer (an egress proxy, a
// network policy). Production code paths that reach arbitrary customer-supplied
// URLs must pass a real guard instead.
func AllowAnyURL() URLGuard { return func(context.Context, *url.URL) error { return nil } }

// ApprovalPolicy decides whether tools discovered from a server are gated
// behind the orchestrator's approval prompt.
type ApprovalPolicy int

const (
	// ApprovalAlways gates every tool from the server. It is the zero value, so
	// a caller that forgets to think about approval fails closed.
	//
	// Why this is the default: MCP gives a client no way to tell a read from a
	// write. A name and a description are all a tool ships, both written by the
	// third party that also implements the tool. Later protocol revisions add
	// annotations.readOnlyHint, but that is the server's self-declaration about
	// its own safety — precisely the claim an attacker would forge — so this
	// package does not use it to lower a gate. The party who can classify the
	// risk is the operator who chose to configure the server URL, and this
	// policy is where they record that judgement.
	ApprovalAlways ApprovalPolicy = iota

	// ApprovalNever ungates the server's tools. Set it only for a server the
	// operator has vetted — typically their own business-data endpoint, whose
	// tools exist to be called autonomously by an unattended chatbot pane.
	ApprovalNever
)

// Config describes one MCP server connection.
type Config struct {
	// Name is a short alias for the server. It prefixes every discovered tool
	// name so two servers exposing "search" do not collide in one registry, and
	// it identifies the server in errors.
	Name string

	// URL is the Streamable-HTTP MCP endpoint (typically ".../mcp").
	URL string

	// Headers are sent on every request — this is where an Authorization
	// header for the customer's server goes.
	Headers map[string]string

	// Guard authorizes the endpoint. Required; see URLGuard.
	Guard URLGuard

	// Timeout bounds one round trip. Zero uses DefaultTimeout.
	Timeout time.Duration

	// MaxResponseBytes caps one response body. Zero uses
	// DefaultMaxResponseBytes; a body over the cap is an error, never a
	// truncated parse.
	MaxResponseBytes int64

	// MaxTools bounds adopted tools. Zero uses DefaultMaxTools.
	MaxTools int

	// Approval selects the approval policy. The zero value is ApprovalAlways.
	Approval ApprovalPolicy

	// HTTPClient overrides the transport client. When nil a client is built
	// with the guard installed as its redirect checker. A supplied client has
	// its CheckRedirect replaced for the same reason.
	HTTPClient *http.Client
}

func (c *Config) applyDefaults() {
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if c.MaxTools <= 0 {
		c.MaxTools = DefaultMaxTools
	}
}

// Client is a protocol-level MCP client over a single Streamable-HTTP
// connection. It performs the handshake, lists tools, and calls them. It is
// safe for concurrent use.
type Client struct {
	cfg Config
	t   *httpTransport

	mu         sync.Mutex
	nextID     int64
	serverInfo implementation
	caps       serverCapabilities
	dropped    int
}

// New validates the configuration and builds a client. It performs no I/O; call
// Initialize (or use Connect) to reach the server.
func New(cfg Config) (*Client, error) {
	if cfg.Guard == nil {
		// Fail closed. See the URLGuard doc comment for why this is not
		// defaulted to allow-all.
		return nil, fmt.Errorf("mcpclient: Config.Guard is required (pass the caller's egress policy, or AllowAnyURL to opt out explicitly)")
	}
	endpoint, err := parseEndpoint(cfg.URL)
	if err != nil {
		return nil, err
	}
	cfg.applyDefaults()

	// Copy rather than mutate: installing the redirect checker on a client the
	// caller also uses elsewhere would silently impose this guard on their other
	// requests. The copy shares the Transport, so connection pooling is kept.
	httpClient := &http.Client{}
	if cfg.HTTPClient != nil {
		c := *cfg.HTTPClient
		httpClient = &c
	}
	httpClient.CheckRedirect = guardedRedirectChecker(cfg.Guard, maxRedirects)

	return &Client{
		cfg: cfg,
		t: &httpTransport{
			name:     cfg.Name,
			endpoint: endpoint,
			headers:  cfg.Headers,
			guard:    cfg.Guard,
			client:   httpClient,
			maxBody:  cfg.MaxResponseBytes,
		},
	}, nil
}

// parseEndpoint applies the input validation this package does own: a
// syntactically valid absolute http(s) URL with a host and no embedded
// credentials. Credentials in a URL are rejected rather than forwarded because
// they leak into logs and redirects; an Authorization header via Config.Headers
// is the supported way to authenticate.
func parseEndpoint(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("mcpclient: Config.URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: parse URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("mcpclient: unsupported URL scheme %q (want http or https)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("mcpclient: URL has no host")
	}
	if u.User != nil {
		return nil, fmt.Errorf("mcpclient: URL must not embed credentials; use Config.Headers")
	}
	return u, nil
}

func (c *Client) id() json.RawMessage {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	return json.RawMessage(strconv.FormatInt(id, 10))
}

// callCtx applies the per-request timeout on top of the caller's context. The
// caller's own cancellation still wins when it fires first, so a visitor
// abandoning a chat cancels the in-flight tool call.
func (c *Client) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.cfg.Timeout)
}

// Initialize performs the MCP handshake: "initialize", then the
// "notifications/initialized" acknowledgement.
func (c *Client) Initialize(ctx context.Context) error {
	params := initializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    clientCapabilities{},
		ClientInfo:      implementation{Name: "rysh", Version: clientVersion},
	}
	raw, _ := json.Marshal(params)

	rctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.t.roundTrip(rctx, jsonrpcRequest{ID: c.id(), Method: "initialize", Params: raw})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	var res initializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return fmt.Errorf("initialize result: %w", err)
	}
	c.mu.Lock()
	c.serverInfo = res.ServerInfo
	c.caps = res.Capabilities
	c.mu.Unlock()

	// Best-effort acknowledgement; servers tolerate its absence but expect it.
	nctx, ncancel := c.callCtx(ctx)
	defer ncancel()
	_ = c.t.notify(nctx, jsonrpcRequest{Method: notificationInitialized})
	return nil
}

// ServerInfo returns the name/version reported by the server (valid after
// Initialize).
func (c *Client) ServerInfo() (name, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverInfo.Name, c.serverInfo.Version
}

// ListTools returns the tools the server advertises, following nextCursor
// pagination. The result is capped at Config.MaxTools; DroppedTools reports how
// many were discarded so a caller can log the cap rather than silently present
// a partial catalog as complete.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	c.mu.Lock()
	c.dropped = 0
	c.mu.Unlock()

	var all []Tool
	cursor := ""
	for page := 0; page < maxToolPages; page++ {
		raw, _ := json.Marshal(toolsListParams{Cursor: cursor})

		rctx, cancel := c.callCtx(ctx)
		resp, err := c.t.roundTrip(rctx, jsonrpcRequest{ID: c.id(), Method: "tools/list", Params: raw})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		var res toolsListResult
		if err := json.Unmarshal(resp.Result, &res); err != nil {
			return nil, fmt.Errorf("tools/list result: %w", err)
		}
		all = append(all, res.Tools...)

		if len(all) >= c.cfg.MaxTools {
			c.mu.Lock()
			c.dropped = len(all) - c.cfg.MaxTools
			c.mu.Unlock()
			return all[:c.cfg.MaxTools], nil
		}
		if res.NextCursor == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
	return nil, fmt.Errorf("tools/list exceeded %d pages (cursor loop?)", maxToolPages)
}

// DroppedTools reports how many tools the MaxTools cap discarded on the last
// ListTools. It counts only tools already fetched when the cap tripped —
// remaining pages are never requested, so a non-zero value means "at least this
// many". Callers should log it: a silently truncated catalog reads to an
// operator as "the server only has these tools".
func (c *Client) DroppedTools() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// CallTool invokes a tool by its server-side name with raw JSON arguments.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	raw, _ := json.Marshal(callToolParams{Name: name, Arguments: args})

	rctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.t.roundTrip(rctx, jsonrpcRequest{ID: c.id(), Method: "tools/call", Params: raw})
	if err != nil {
		return nil, err
	}
	var res CallToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return nil, fmt.Errorf("tools/call result: %w", err)
	}
	return &res, nil
}

// Close releases the transport.
func (c *Client) Close() error { return c.t.close() }

// Connect is the one-call path a caller wants: validate, handshake, list tools,
// and return them adapted to the shared ToolExecutor interface. On error the
// client is closed before returning.
func Connect(ctx context.Context, cfg Config) (*Client, []sharedtools.ToolExecutor, error) {
	c, err := New(cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := c.Initialize(ctx); err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	list, err := c.ListTools(ctx)
	if err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	return c, c.Executors(list), nil
}
