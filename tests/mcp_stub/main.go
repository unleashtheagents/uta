// mcp_stub is a tiny MCP server used as a fixture for the client_test
// subprocess smoke test. It registers a single "echo" tool that returns
// its argument JSON verbatim, plus a "boom" tool that always returns an
// IsError ToolResult so the client's error path is exercised end-to-end.
//
// This binary is NOT part of the production build — it lives under
// tests/ so `go test ./...` compiles it (catching API drift) without
// shipping it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/unleashtheagents/uta/internal/mcp"
)

func main() {
	s := mcp.NewServer("uta-mcp-stub", "0.0.1")
	if err := s.RegisterTool(mcp.Tool{
		Name:        "echo",
		Description: "echo the arguments back as text",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"additionalProperties":true}`),
	}, func(_ context.Context, args json.RawMessage) mcp.ToolResult {
		return mcp.TextResult(string(args))
	}); err != nil {
		fmt.Fprintln(os.Stderr, "register echo:", err)
		os.Exit(1)
	}
	if err := s.RegisterTool(mcp.Tool{
		Name:        "boom",
		Description: "always reports an IsError ToolResult",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
	}, func(_ context.Context, _ json.RawMessage) mcp.ToolResult {
		return mcp.ErrorResult("intentional failure")
	}); err != nil {
		fmt.Fprintln(os.Stderr, "register boom:", err)
		os.Exit(1)
	}
	if err := s.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}
