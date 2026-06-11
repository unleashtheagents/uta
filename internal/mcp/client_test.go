package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipedServer wires an mcp.Server through two io.Pipe pairs to the
// returned Client. Returns the live Client and a cleanup func.
//
// Pipe topology:
//
//	client.writer  -> clientToServerW -> clientToServerR  -> server.read
//	server.write   -> serverToClientW -> serverToClientR  -> client.reader
//
// Both pipes are closed by the cleanup, which also stops the server's
// Serve goroutine.
func pipedServer(t *testing.T, register func(*Server)) (*Client, func()) {
	t.Helper()
	s := NewServer("uta-mcp-test", "0.0.0-test")
	register(s)

	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = s.Serve(ctx, clientToServerR, serverToClientW)
	}()

	c := NewClient(serverToClientR, clientToServerW)
	cleanup := func() {
		_ = c.Close()
		cancel()
		// Closing the client's writer signals EOF to the server's reader,
		// which should make Serve return. The cancel above is a belt &
		// braces for environments that don't propagate the pipe close.
		_ = clientToServerW.Close()
		_ = serverToClientW.Close()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Logf("warn: server goroutine did not exit within 2s")
		}
	}
	return c, cleanup
}

func TestClient_InitializeAndListTools(t *testing.T) {
	c, cleanup := pipedServer(t, func(s *Server) {
		_ = s.RegisterTool(Tool{Name: "alpha", Description: "first"}, func(context.Context, json.RawMessage) ToolResult {
			return TextResult("a")
		})
		_ = s.RegisterTool(Tool{Name: "beta", Description: "second"}, func(context.Context, json.RawMessage) ToolResult {
			return TextResult("b")
		})
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	name, version := c.ServerInfo()
	if name != "uta-mcp-test" || version != "0.0.0-test" {
		t.Errorf("ServerInfo = %q,%q", name, version)
	}

	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "alpha" || tools[1].Name != "beta" {
		t.Errorf("ListTools = %+v", tools)
	}
}

func TestClient_CallToolEchoesArguments(t *testing.T) {
	c, cleanup := pipedServer(t, func(s *Server) {
		_ = s.RegisterTool(Tool{Name: "echo"}, func(_ context.Context, args json.RawMessage) ToolResult {
			return TextResult(string(args))
		})
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := c.CallTool(ctx, "echo", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatal("did not expect IsError")
	}
	if len(res.Content) != 1 || res.Content[0].Text != `{"x":1}` {
		t.Errorf("Content = %+v", res.Content)
	}
}

func TestClient_CallToolDefaultsEmptyArgsToObject(t *testing.T) {
	var got string
	c, cleanup := pipedServer(t, func(s *Server) {
		_ = s.RegisterTool(Tool{Name: "echo"}, func(_ context.Context, args json.RawMessage) ToolResult {
			got = string(args)
			return TextResult("")
		})
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.CallTool(ctx, "echo", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got != "{}" {
		t.Errorf("server saw args=%q, want \"{}\"", got)
	}
}

func TestClient_CallToolPropagatesIsError(t *testing.T) {
	c, cleanup := pipedServer(t, func(s *Server) {
		_ = s.RegisterTool(Tool{Name: "boom"}, func(context.Context, json.RawMessage) ToolResult {
			return ErrorResult("nope")
		})
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := c.CallTool(ctx, "boom", nil)
	if err != nil {
		t.Fatalf("CallTool: unexpected transport error: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true")
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "nope") {
		t.Errorf("Content = %+v", res.Content)
	}
}

func TestClient_CallUnknownToolReturnsError(t *testing.T) {
	c, cleanup := pipedServer(t, func(s *Server) {})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	_, err := c.CallTool(ctx, "nope", nil)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should reference tool name: %v", err)
	}
}

func TestClient_CallToolEmptyNameRejected(t *testing.T) {
	c, cleanup := pipedServer(t, func(s *Server) {})
	defer cleanup()
	_, err := c.CallTool(context.Background(), "", nil)
	if err == nil {
		t.Fatal("expected error for empty tool name")
	}
}

func TestClient_ConcurrentCalls(t *testing.T) {
	c, cleanup := pipedServer(t, func(s *Server) {
		_ = s.RegisterTool(Tool{Name: "echo"}, func(_ context.Context, a json.RawMessage) ToolResult {
			return TextResult(string(a))
		})
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	var wg sync.WaitGroup
	const N = 20
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload, _ := json.Marshal(map[string]int{"i": i})
			res, err := c.CallTool(ctx, "echo", payload)
			if err != nil {
				errs <- err
				return
			}
			if !strings.Contains(res.Content[0].Text, string(payload)) {
				errs <- nil
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Errorf("concurrent call error: %v", e)
		}
	}
}

func TestClient_CloseAfterClose(t *testing.T) {
	c, cleanup := pipedServer(t, func(s *Server) {})
	defer cleanup()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Second Close must be a no-op.
	if err := c.Close(); err != nil {
		t.Fatalf("Close (second): %v", err)
	}
}

func TestClient_CallAfterCloseFails(t *testing.T) {
	c, cleanup := pipedServer(t, func(s *Server) {})
	defer cleanup()
	_ = c.Close()
	_, err := c.CallTool(context.Background(), "anything", nil)
	if err == nil {
		t.Fatal("expected error after Close")
	}
}

func TestClient_ContextCancelDuringCall(t *testing.T) {
	// Server that never replies — we want CallTool to unblock when ctx
	// is cancelled rather than hanging on a dead pending channel.
	c, cleanup := pipedServer(t, func(s *Server) {
		_ = s.RegisterTool(Tool{Name: "hang"}, func(ctx context.Context, _ json.RawMessage) ToolResult {
			<-ctx.Done()
			return TextResult("late")
		})
	})
	defer cleanup()

	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := c.CallTool(ctx, "hang", nil)
	if err == nil {
		t.Fatal("expected timeout/canceled error")
	}
}

// TestStdioClient_AgainstFixtureStub launches the tests/mcp_stub binary
// as a subprocess and exercises the full stdio plumbing end-to-end. This
// is the integration counterpart to the in-process pipe tests above. It
// requires the `go` toolchain on PATH; if `go build` is unavailable the
// test skips rather than failing.
func TestStdioClient_AgainstFixtureStub(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test skipped in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping subprocess fixture test")
	}
	moduleRoot, err := findModuleRoot()
	if err != nil {
		t.Skipf("cannot locate module root: %v", err)
	}
	stubSrc := filepath.Join(moduleRoot, "tests", "mcp_stub")
	if _, err := os.Stat(stubSrc); err != nil {
		t.Skipf("fixture %s not present: %v", stubSrc, err)
	}

	tmp := t.TempDir()
	binName := "mcp_stub"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmp, binName)
	buildCmd := exec.Command("go", "build", "-o", binPath, "./tests/mcp_stub")
	buildCmd.Dir = moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build stub: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c, err := NewStdioClient(ctx, binPath, nil, map[string]string{"UTA_TEST_STUB": "1"})
	if err != nil {
		t.Fatalf("NewStdioClient: %v", err)
	}
	defer c.Close()

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	name, _ := c.ServerInfo()
	if name != "uta-mcp-stub" {
		t.Errorf("server name = %q, want %q", name, "uta-mcp-stub")
	}

	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	wantTools := map[string]bool{"echo": false, "boom": false}
	for _, tl := range tools {
		if _, ok := wantTools[tl.Name]; ok {
			wantTools[tl.Name] = true
		}
	}
	for name, seen := range wantTools {
		if !seen {
			t.Errorf("missing tool %q in advertised set %+v", name, tools)
		}
	}

	res, err := c.CallTool(ctx, "echo", json.RawMessage(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}
	if res.IsError || !strings.Contains(res.Content[0].Text, `"hello":"world"`) {
		t.Errorf("echo result = %+v", res)
	}

	boom, err := c.CallTool(ctx, "boom", nil)
	if err != nil {
		t.Fatalf("CallTool boom (transport): %v", err)
	}
	if !boom.IsError {
		t.Errorf("expected boom to return IsError=true, got %+v", boom)
	}
}

// TestStdioClient_MissingBinary verifies the clean-error path the
// acceptance criterion calls out: "Without OAuth: clean error, no panic."
// A non-existent binary maps to a clean exec error from NewStdioClient,
// not a nil-pointer panic later inside Initialize.
func TestStdioClient_MissingBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := NewStdioClient(ctx, "this-binary-cannot-possibly-exist-uta-mcp", nil, nil)
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

// findModuleRoot walks up from the test file's directory looking for
// the nearest go.mod. Returns the dir containing it.
func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
