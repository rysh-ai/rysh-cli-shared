// SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// ---------------------------------------------------------------------------
// fake MCP server
// ---------------------------------------------------------------------------

// fakeMCP is an httptest-backed MCP server speaking Streamable HTTP. Each field
// lets one test bend a single behaviour (a slow reply, an oversized body, an
// error result) without forking the whole handler.
type fakeMCP struct {
	tools map[string][]Tool // cursor ("" = first page) -> tools on that page
	next  map[string]string // cursor -> nextCursor

	callResult  func(name string, args json.RawMessage) (interface{}, bool) // result, isError
	callRPCErr  *rpcError
	delay       time.Duration
	padBody     int  // pad every response with this many bytes of filler
	useSSE      bool // reply as text/event-stream instead of application/json
	failInit    bool
	serverName  string
	protocolVer string

	mu       sync.Mutex
	methods  []string          // every method received, in order
	initReq  initializeParams  // params of the initialize call
	lastCall callToolParams    // params of the last tools/call
	headers  map[string]string // headers of the last request
}

func newFakeMCP() *fakeMCP {
	return &fakeMCP{
		tools:       map[string][]Tool{},
		next:        map[string]string{},
		serverName:  "fake-mcp",
		protocolVer: ProtocolVersion,
		headers:     map[string]string{},
	}
}

func (f *fakeMCP) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

func (f *fakeMCP) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeMCP) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.methods = append(f.methods, req.Method)
	for k := range r.Header {
		f.headers[k] = r.Header.Get(k)
	}
	f.mu.Unlock()

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
	}

	// Notifications carry no id and expect no JSON-RPC reply.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var result interface{}
	var rpcErr *rpcError

	switch req.Method {
	case "initialize":
		f.mu.Lock()
		_ = json.Unmarshal(req.Params, &f.initReq)
		f.mu.Unlock()
		if f.failInit {
			rpcErr = &rpcError{Code: -32603, Message: "initialize refused"}
			break
		}
		result = initializeResult{
			ProtocolVersion: f.protocolVer,
			Capabilities:    serverCapabilities{Tools: &toolsCapability{ListChanged: true}},
			ServerInfo:      implementation{Name: f.serverName, Version: "9.9.9"},
		}
	case "tools/list":
		var p toolsListParams
		_ = json.Unmarshal(req.Params, &p)
		result = toolsListResult{Tools: f.tools[p.Cursor], NextCursor: f.next[p.Cursor]}
	case "tools/call":
		var p callToolParams
		_ = json.Unmarshal(req.Params, &p)
		f.mu.Lock()
		f.lastCall = p
		f.mu.Unlock()
		if f.callRPCErr != nil {
			rpcErr = f.callRPCErr
			break
		}
		if f.callResult != nil {
			res, isErr := f.callResult(p.Name, p.Arguments)
			result = CallToolResult{
				Content: []contentBlock{{Type: "text", Text: fmt.Sprint(res)}},
				IsError: isErr,
			}
			break
		}
		result = CallToolResult{Content: []contentBlock{{Type: "text", Text: "ok:" + p.Name}}}
	default:
		rpcErr = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}

	env := map[string]interface{}{"jsonrpc": "2.0", "id": json.RawMessage(req.ID)}
	if rpcErr != nil {
		env["error"] = rpcErr
	} else {
		env["result"] = result
	}
	if f.padBody > 0 {
		env["_filler"] = strings.Repeat("x", f.padBody)
	}
	body, _ := json.Marshal(env)

	if f.useSSE {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, ": keep-alive\nevent: message\ndata: %s\n\n", body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// testConfig is the base config every test starts from: it points at the fake
// server and injects the permissive guard (httptest binds to loopback, which a
// real egress policy correctly refuses).
func testConfig(url string) Config {
	return Config{Name: "fake", URL: url, Guard: AllowAnyURL()}
}

func mustClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func specByName(execs []sharedtools.ToolExecutor, name string) (sharedtools.ToolExecutor, bool) {
	for _, e := range execs {
		if e.Spec().Name == name {
			return e, true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestInitializeHandshake(t *testing.T) {
	f := newFakeMCP()
	srv := f.start(t)

	cfg := testConfig(srv.URL)
	cfg.Headers = map[string]string{"Authorization": "Bearer sekret"}
	c := mustClient(t, cfg)

	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	name, version := c.ServerInfo()
	if name != "fake-mcp" || version != "9.9.9" {
		t.Errorf("ServerInfo() = %q/%q, want fake-mcp/9.9.9", name, version)
	}

	f.mu.Lock()
	gotVer := f.initReq.ProtocolVersion
	gotClient := f.initReq.ClientInfo.Name
	auth := f.headers["Authorization"]
	f.mu.Unlock()

	if gotVer != ProtocolVersion {
		t.Errorf("initialize protocolVersion = %q, want %q", gotVer, ProtocolVersion)
	}
	if gotClient == "" {
		t.Error("initialize sent no clientInfo.name")
	}
	if auth != "Bearer sekret" {
		t.Errorf("Authorization header = %q, want the configured value", auth)
	}

	// The handshake must be acknowledged: MCP servers expect
	// notifications/initialized before any other request.
	methods := f.seen()
	if len(methods) < 2 || methods[0] != "initialize" || methods[1] != "notifications/initialized" {
		t.Errorf("method sequence = %v, want [initialize notifications/initialized ...]", methods)
	}
}

func TestInitializeServerError(t *testing.T) {
	f := newFakeMCP()
	f.failInit = true
	srv := f.start(t)

	c := mustClient(t, testConfig(srv.URL))
	err := c.Initialize(context.Background())
	if err == nil {
		t.Fatal("Initialize succeeded against a server that refused it")
	}
	if !strings.Contains(err.Error(), "initialize refused") {
		t.Errorf("error = %v, want the server's message", err)
	}
}

func TestListToolsMapsToSpecs(t *testing.T) {
	f := newFakeMCP()
	// Two pages, to prove nextCursor pagination is followed.
	f.tools[""] = []Tool{{
		Name:        "list_classes",
		Description: "List tango classes for a date",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"date":{"type":"string"}},"required":["date"]}`),
	}}
	f.next[""] = "page2"
	f.tools["page2"] = []Tool{{
		Name:        "log timesheet!",
		Description: "Log hours",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}
	srv := f.start(t)

	c := mustClient(t, testConfig(srv.URL))
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	list, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListTools returned %d tools, want 2 (pagination not followed?)", len(list))
	}

	execs := c.Executors(list)
	if len(execs) != 2 {
		t.Fatalf("Executors returned %d, want 2", len(execs))
	}

	e, ok := specByName(execs, "fake_list_classes")
	if !ok {
		var names []string
		for _, x := range execs {
			names = append(names, x.Spec().Name)
		}
		t.Fatalf("no executor named fake_list_classes; got %v", names)
	}
	spec := e.Spec()
	if spec.Description != "List tango classes for a date" {
		t.Errorf("Description = %q, want the MCP description", spec.Description)
	}
	// The MCP inputSchema is a JSON Schema and must reach ToolSpec.Parameters
	// intact — a lossy round-trip would drop "required" and the model would
	// call the tool with missing arguments.
	var got, want map[string]interface{}
	_ = json.Unmarshal(spec.Parameters, &got)
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{"date":{"type":"string"}},"required":["date"]}`), &want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Parameters = %s, want the inputSchema verbatim", spec.Parameters)
	}

	// "log timesheet!" is not a legal Anthropic tool name; it must be coerced.
	if _, ok := specByName(execs, "fake_log_timesheet_"); !ok {
		var names []string
		for _, x := range execs {
			names = append(names, x.Spec().Name)
		}
		t.Errorf("tool name was not sanitized for the tool API; got %v", names)
	}
}

func TestRequiresApprovalDefaultsToTrue(t *testing.T) {
	f := newFakeMCP()
	f.tools[""] = []Tool{{Name: "delete_everything", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := f.start(t)

	// Default policy (zero value) — an unvetted third-party server.
	c := mustClient(t, testConfig(srv.URL))
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	list, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	e := c.Executors(list)[0]
	if !e.RequiresApproval(nil) {
		t.Error("RequiresApproval = false by default; an unvetted MCP tool must be gated")
	}
	if !e.Spec().RequiresApproval {
		t.Error("Spec().RequiresApproval = false by default; must match RequiresApproval()")
	}

	// Explicit opt-out for a server the operator vetted.
	cfg := testConfig(srv.URL)
	cfg.Approval = ApprovalNever
	c2 := mustClient(t, cfg)
	if err := c2.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	list2, err := c2.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if c2.Executors(list2)[0].RequiresApproval(nil) {
		t.Error("ApprovalNever did not disable the approval gate")
	}
}

func TestCallToolRoundTrip(t *testing.T) {
	f := newFakeMCP()
	f.tools[""] = []Tool{{Name: "list_classes", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	f.callResult = func(name string, args json.RawMessage) (interface{}, bool) {
		return fmt.Sprintf("classes for %s", string(args)), false
	}
	srv := f.start(t)

	c := mustClient(t, testConfig(srv.URL))
	_, execs, err := Connect(context.Background(), testConfig(srv.URL))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_ = c

	if len(execs) != 1 {
		t.Fatalf("Connect returned %d executors, want 1", len(execs))
	}
	out, err := execs[0].Execute(context.Background(), json.RawMessage(`{"date":"2026-08-08"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("Execute returned tool error: %s", out.Error)
	}
	if !strings.Contains(out.Content, "2026-08-08") {
		t.Errorf("Content = %q, want the server's rendered text", out.Content)
	}
	if out.Metadata["mcp_tool"] != "list_classes" {
		t.Errorf("Metadata[mcp_tool] = %q, want the server-side tool name", out.Metadata["mcp_tool"])
	}

	// tools/call must carry the server-side name, not the prefixed registry name.
	f.mu.Lock()
	called := f.lastCall.Name
	f.mu.Unlock()
	if called != "list_classes" {
		t.Errorf("tools/call name = %q, want the unprefixed remote name", called)
	}
}

func TestCallToolErrorResult(t *testing.T) {
	f := newFakeMCP()
	f.tools[""] = []Tool{{Name: "flaky", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	f.callResult = func(string, json.RawMessage) (interface{}, bool) {
		return "no such class", true // isError: the tool ran and reported failure
	}
	srv := f.start(t)

	_, execs, err := Connect(context.Background(), testConfig(srv.URL))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	out, err := execs[0].Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute returned a Go error for an isError result: %v", err)
	}
	if out.Error == "" {
		t.Fatal("isError result did not surface as ToolOutput.Error")
	}
	if !strings.Contains(out.Error, "no such class") {
		t.Errorf("Error = %q, want the server's text so the model can recover", out.Error)
	}
}

func TestCallToolRPCError(t *testing.T) {
	f := newFakeMCP()
	f.tools[""] = []Tool{{Name: "gone", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	f.callRPCErr = &rpcError{Code: -32602, Message: "invalid params"}
	srv := f.start(t)

	_, execs, err := Connect(context.Background(), testConfig(srv.URL))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	out, err := execs[0].Execute(context.Background(), json.RawMessage(`{"bad":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Error == "" {
		t.Fatal("JSON-RPC error did not surface as ToolOutput.Error")
	}
	if out.ErrorKind != sharedtools.ErrKindValidation {
		t.Errorf("ErrorKind = %q, want %q for -32602", out.ErrorKind, sharedtools.ErrKindValidation)
	}
}

func TestCallToolTimeout(t *testing.T) {
	f := newFakeMCP()
	f.tools[""] = []Tool{{Name: "slow", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := f.start(t)

	cfg := testConfig(srv.URL)
	cfg.Timeout = 5 * time.Second // generous for connect/list
	_, execs, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Now make the server hang and re-run with a tight per-call timeout.
	f.delay = 3 * time.Second
	cfg2 := testConfig(srv.URL)
	cfg2.Timeout = 150 * time.Millisecond
	c2 := mustClient(t, cfg2)

	start := time.Now()
	out, err := c2.Executors([]Tool{{Name: "slow", InputSchema: json.RawMessage(`{"type":"object"}`)}})[0].
		Execute(context.Background(), nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Error == "" {
		t.Fatal("a timed-out call did not report an error")
	}
	if out.ErrorKind != sharedtools.ErrKindTimeout {
		t.Errorf("ErrorKind = %q, want %q", out.ErrorKind, sharedtools.ErrKindTimeout)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Execute took %s; the per-call timeout did not bound it", elapsed)
	}
	_ = execs
}

func TestResponseSizeCap(t *testing.T) {
	f := newFakeMCP()
	f.tools[""] = []Tool{{Name: "chatty", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	f.padBody = 64 * 1024
	srv := f.start(t)

	cfg := testConfig(srv.URL)
	cfg.MaxResponseBytes = 4096
	c := mustClient(t, cfg)

	err := c.Initialize(context.Background())
	if err == nil {
		t.Fatal("an oversized response was accepted; the size cap did not trip")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %v, want it to name the size cap", err)
	}

	// And the cap must not fire on a normal-sized response.
	f.padBody = 0
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize under the cap failed: %v", err)
	}
}

func TestMaxToolsBounded(t *testing.T) {
	f := newFakeMCP()
	for i := 0; i < 50; i++ {
		f.tools[""] = append(f.tools[""], Tool{
			Name:        fmt.Sprintf("tool_%02d", i),
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}
	srv := f.start(t)

	cfg := testConfig(srv.URL)
	cfg.MaxTools = 10
	c := mustClient(t, cfg)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	list, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(list) != 10 {
		t.Errorf("ListTools returned %d tools, want them capped at 10", len(list))
	}
}

func TestCursorLoopIsBounded(t *testing.T) {
	f := newFakeMCP()
	f.tools[""] = []Tool{{Name: "a", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	f.next[""] = "" // set below to point at itself
	f.next[""] = "loop"
	f.tools["loop"] = []Tool{{Name: "b", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	f.next["loop"] = "loop" // a server that never stops paging
	srv := f.start(t)

	cfg := testConfig(srv.URL)
	cfg.MaxTools = 1000
	c := mustClient(t, cfg)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.ListTools(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("ListTools did not terminate against a cursor loop")
	}
}

func TestSSEResponse(t *testing.T) {
	f := newFakeMCP()
	f.useSSE = true
	f.tools[""] = []Tool{{Name: "sse_tool", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := f.start(t)

	_, execs, err := Connect(context.Background(), testConfig(srv.URL))
	if err != nil {
		t.Fatalf("Connect over SSE: %v", err)
	}
	if len(execs) != 1 {
		t.Fatalf("got %d executors over SSE, want 1", len(execs))
	}
	out, err := execs[0].Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute over SSE: %v", err)
	}
	if !strings.Contains(out.Content, "ok:sse_tool") {
		t.Errorf("Content = %q, want the SSE-framed result", out.Content)
	}
}

func TestGuardIsRequired(t *testing.T) {
	// A nil guard must be refused, not silently treated as "allow everything".
	// This is the design-016 gap 3 lesson: an unset egress policy is how the
	// custom HTTP tool shipped able to reach the cloud metadata endpoint.
	_, err := New(Config{Name: "x", URL: "http://example.invalid/mcp"})
	if err == nil {
		t.Fatal("New accepted a nil Guard; egress policy must be explicit")
	}
	if !strings.Contains(err.Error(), "Guard") {
		t.Errorf("error = %v, want it to name the missing Guard", err)
	}
}

func TestGuardRejectsURL(t *testing.T) {
	f := newFakeMCP()
	srv := f.start(t)

	cfg := testConfig(srv.URL)
	cfg.Guard = func(context.Context, *url.URL) error { return fmt.Errorf("blocked by policy") }
	c := mustClient(t, cfg)

	err := c.Initialize(context.Background())
	if err == nil {
		t.Fatal("Initialize succeeded against a guard-rejected URL")
	}
	if !strings.Contains(err.Error(), "blocked by policy") {
		t.Errorf("error = %v, want the guard's reason", err)
	}
	if got := f.seen(); len(got) != 0 {
		t.Errorf("guard-rejected request still reached the server: %v", got)
	}
}

func TestBadURLRejected(t *testing.T) {
	for _, raw := range []string{"", "ftp://example.com/mcp", "http://user:pw@example.com/mcp"} {
		if _, err := New(Config{Name: "x", URL: raw, Guard: AllowAnyURL()}); err == nil {
			t.Errorf("New(%q) succeeded, want rejection", raw)
		}
	}
}
