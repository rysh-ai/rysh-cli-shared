// SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Integration tests against the REAL reference MCP servers in the in-repo
// rysh-mcp-samples tree. They are not build-tagged — a client that only ever
// talks to its own fake proves the fake, not the protocol — but every step that
// depends on something outside this module (the samples checkout, the Go
// toolchain, a free port) skips rather than fails. A missing local dependency
// must never redden the suite.
//
// Which sample each test uses, and why:
//
//   - mcp-rest-api-wrapper + sample-rest-server speak Streamable HTTP, which is
//     the transport this package implements, so TestSampleHTTPWrapper is the
//     true end-to-end test: real socket, real HTTP framing, real server.
//   - mcp-server (the one MCP-GUIDE.md and test-mcp.sh document) is stdio-only
//     — "no port, no network", per the guide. It cannot be reached over HTTP at
//     all. TestSampleStdioServerOverBridge still exercises it, by piping its
//     stdin/stdout through a local HTTP shim: everything above the framing —
//     handshake, tools/list schemas, tools/call results — is that server's real
//     implementation, which is what makes it worth asserting against.

// findSamplesDir walks up from the working directory to locate rysh-mcp-samples.
func findSamplesDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Skipf("getwd: %v", err)
	}
	for {
		cand := filepath.Join(dir, "rysh-mcp-samples")
		if _, err := os.Stat(filepath.Join(cand, "mcp-server", "main.go")); err == nil {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("rysh-mcp-samples not found (submodule not checked out); skipping sample integration test")
		}
		dir = parent
	}
}

// buildSample compiles a sample into a temp dir, skipping if it will not build.
func buildSample(t *testing.T, sampleDir string) string {
	t.Helper()
	// Compiling the samples (gin and friends) dominates these tests' runtime,
	// so -short skips them; the default `go test ./...` still runs them.
	if testing.Short() {
		t.Skip("-short: skipping sample integration test (compiles the sample servers)")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping sample integration test")
	}
	bin := filepath.Join(t.TempDir(), "sample-bin")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	cmd.Dir = sampleDir
	// The samples are their own modules, outside this repo's go.work.
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not build sample %s: %v\n%s", sampleDir, err, out)
	}
	return bin
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitHealthy(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Skipf("sample service at %s did not become healthy within %s; skipping", url, timeout)
}

// TestSampleHTTPWrapper drives the real Streamable-HTTP sample end to end.
func TestSampleHTTPWrapper(t *testing.T) {
	samples := findSamplesDir(t)
	restBin := buildSample(t, filepath.Join(samples, "sample-rest-server"))
	wrapBin := buildSample(t, filepath.Join(samples, "mcp-rest-api-wrapper"))

	restPort := freePort(t)
	mcpPort := freePort(t)

	rest := exec.Command(restBin)
	rest.Env = append(os.Environ(), fmt.Sprintf("MCP_LISTEN_ADDR=:%d", restPort))
	var restLog lockedBuffer
	rest.Stdout, rest.Stderr = &restLog, &restLog
	if err := rest.Start(); err != nil {
		t.Skipf("could not start sample REST server: %v", err)
	}
	restExit := make(chan error, 1)
	go func() { restExit <- rest.Wait() }()
	defer func() { _ = rest.Process.Kill() }()
	t.Cleanup(func() {
		select {
		case err := <-restExit:
			t.Logf("sample REST server exited early: %v", err)
		default:
		}
		if s := restLog.String(); s != "" {
			t.Logf("sample REST server output:\n%s", s)
		}
	})

	// Wait for the REST backend before the wrapper: the wrapper's /healthz
	// reports only its own liveness, so a wrapper that is up while its backend
	// is still binding answers tools/call with a connection-refused error.
	// Generous: on a cold build cache the machine is still busy compiling, and a
	// slow-to-bind backend should not be reported as a broken client.
	waitHealthy(t, fmt.Sprintf("http://127.0.0.1:%d/api/weather?city=tokyo", restPort), 30*time.Second)

	wrap := exec.Command(wrapBin)
	wrap.Env = append(os.Environ(),
		fmt.Sprintf("MCP_LISTEN_ADDR=:%d", mcpPort),
		fmt.Sprintf("REST_API_URL=http://127.0.0.1:%d", restPort),
	)
	if err := wrap.Start(); err != nil {
		t.Skipf("could not start sample MCP wrapper: %v", err)
	}
	defer func() { _ = wrap.Process.Kill(); _ = wrap.Wait() }()

	waitHealthy(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", mcpPort), 15*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := Config{
		Name:  "weather",
		URL:   fmt.Sprintf("http://127.0.0.1:%d/mcp", mcpPort),
		Guard: AllowAnyURL(), // the sample binds to loopback; a real policy refuses that
	}
	c, execs, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect to sample wrapper: %v", err)
	}
	defer c.Close()

	name, version := c.ServerInfo()
	if name == "" {
		t.Error("sample server reported no serverInfo.name")
	}
	t.Logf("sample HTTP server: %s %s", name, version)

	if len(execs) < 2 {
		t.Fatalf("got %d tools from the wrapper, want at least 2 (get_weather, convert_units)", len(execs))
	}

	e, ok := specByName(execs, "weather_get_weather")
	if !ok {
		var names []string
		for _, x := range execs {
			names = append(names, x.Spec().Name)
		}
		t.Fatalf("weather_get_weather not among %v", names)
	}
	if len(e.Spec().Parameters) == 0 {
		t.Error("get_weather spec carries no JSON Schema")
	}

	out, err := e.Execute(ctx, json.RawMessage(`{"city":"tokyo"}`))
	if err != nil {
		t.Fatalf("Execute get_weather: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("get_weather reported an error: %s", out.Error)
	}
	if !strings.Contains(strings.ToLower(out.Content), "tokyo") {
		t.Errorf("get_weather(tokyo) = %q, want it to mention tokyo", out.Content)
	}
	t.Logf("http sample get_weather(tokyo) => %s", strings.ReplaceAll(out.Content, "\n", " "))

	// An unknown tool must come back as a readable tool error, not a panic or a
	// dead turn.
	bad := &toolExecutor{client: c, serverName: "weather", remoteName: "no_such_tool"}
	berr, err := bad.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute unknown tool returned a Go error: %v", err)
	}
	if berr.Error == "" {
		t.Error("unknown tool did not report an error to the model")
	}
	t.Logf("unknown tool => kind=%s %s", berr.ErrorKind, berr.Error)
}

// TestSampleStdioServerOverBridge exercises the canonical stdio sample
// (rysh-mcp-samples/mcp-server) through an in-test HTTP shim. Only the framing
// is bridged; the handshake, tool schemas and tool results are that server's
// own. It is what lets this HTTP-only client be checked against the server
// MCP-GUIDE.md and test-mcp.sh document.
func TestSampleStdioServerOverBridge(t *testing.T) {
	samples := findSamplesDir(t)
	bin := buildSample(t, filepath.Join(samples, "mcp-server"))

	br, err := startStdioBridge(bin)
	if err != nil {
		t.Skipf("could not start stdio sample: %v", err)
	}
	defer br.Close()

	srv := httptest.NewServer(http.HandlerFunc(br.serveHTTP))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, execs, err := Connect(ctx, Config{Name: "weather", URL: srv.URL, Guard: AllowAnyURL()})
	if err != nil {
		t.Fatalf("Connect to stdio sample via bridge: %v", err)
	}
	defer c.Close()

	name, version := c.ServerInfo()
	t.Logf("sample stdio server: %s %s", name, version)
	if len(execs) < 2 {
		t.Fatalf("got %d tools from mcp-server, want at least 2", len(execs))
	}

	e, ok := specByName(execs, "weather_get_weather")
	if !ok {
		var names []string
		for _, x := range execs {
			names = append(names, x.Spec().Name)
		}
		t.Fatalf("weather_get_weather not among %v", names)
	}
	out, err := e.Execute(ctx, json.RawMessage(`{"city":"istanbul"}`))
	if err != nil {
		t.Fatalf("Execute get_weather: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("get_weather reported an error: %s", out.Error)
	}
	if !strings.Contains(strings.ToLower(out.Content), "istanbul") {
		t.Errorf("get_weather(istanbul) = %q, want it to mention istanbul", out.Content)
	}
	t.Logf("stdio sample get_weather(istanbul) => %s", strings.ReplaceAll(out.Content, "\n", " "))
}

// lockedBuffer collects a child process's output for diagnostics. The child
// writes from its own goroutine while the test reads, so the mutex is required.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stdioBridge pipes JSON-RPC between an HTTP endpoint and a child process
// speaking newline-delimited JSON-RPC on stdin/stdout. Requests are serialized:
// the sample server answers one line per request, in order.
type stdioBridge struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
	mu  sync.Mutex
}

func startStdioBridge(bin string) (*stdioBridge, error) {
	cmd := exec.Command(bin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// The sample's line-oriented tool output can exceed bufio's default.
	return &stdioBridge{cmd: cmd, in: stdin, out: bufio.NewReaderSize(stdout, 1<<20)}, nil
}

func (b *stdioBridge) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}

	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(body, &probe)

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, err := b.in.Write(append(body, '\n')); err != nil {
		http.Error(w, "write to sample: "+err.Error(), http.StatusBadGateway)
		return
	}
	// A notification (no id) draws no reply on stdout.
	if len(probe.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	line, err := b.out.ReadBytes('\n')
	if err != nil {
		http.Error(w, "read from sample: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(line)
}

func (b *stdioBridge) Close() {
	_ = b.in.Close()
	if b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
	_ = b.cmd.Wait()
}
