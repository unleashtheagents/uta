package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/lsp"
	"github.com/unleashtheagents/uta/internal/steer"
)

// The steer program below fails Check with `unknown function "gret"` on
// line 5 (1-based) — line 4 in the protocol's zero-based positions.
const brokenSteerSrc = `agent fn greet(name: Text) -> Text
  prompt """hello ${name}"""
mission m {
  budget 10k tokens
  emit gret("world")
}`

func TestSteerDiagnostics_BrokenProgram(t *testing.T) {
	diags := steerDiagnostics("file:///broken.steer", brokenSteerSrc)
	if len(diags) == 0 {
		t.Fatal("no diagnostics for a broken program")
	}
	d := diags[0]
	if !strings.Contains(d.Message, `unknown function "gret"`) {
		t.Errorf("message = %q", d.Message)
	}
	if d.Range.Start.Line != 4 {
		t.Errorf("line = %d, want 4 (zero-based)", d.Range.Start.Line)
	}
	if d.Severity != lsp.SeverityError || d.Source != "steer" {
		t.Errorf("diagnostic = %+v", d)
	}
}

// misformattedSteerSrc parses and checks clean but is not canonical:
// the budget is spelled 10000 (fmt writes 10k) and the declarations are
// not separated by a blank line.
const misformattedSteerSrc = `agent fn greet(name: Text) -> Text
  prompt """hello ${name}"""
mission m {
  budget 10000 tokens
  emit greet("world")
}`

// canonicalSteer is what `uta mission fmt` would produce for src; tests
// compare against it rather than a hand-written literal so they assert
// "matches fmt" instead of re-encoding the formatter's choices.
func canonicalSteer(t *testing.T, src string) string {
	t.Helper()
	prog, pd := steer.Parse("t.steer", src)
	if pd != nil {
		t.Fatalf("fixture no longer parses: %s", pd.Render())
	}
	return steer.Format(prog)
}

func TestSteerFormat(t *testing.T) {
	out, ok := steerFormat("file:///m.steer", misformattedSteerSrc)
	if !ok {
		t.Fatal("format refused a clean program")
	}
	if want := canonicalSteer(t, misformattedSteerSrc); out != want {
		t.Errorf("formatted:\n%s\nwant:\n%s", out, want)
	}
	if out == misformattedSteerSrc {
		t.Error("fixture is already canonical; the test proves nothing")
	}

	if _, ok := steerFormat("file:///b.steer", "not a steer program"); ok {
		t.Error("format accepted a program with parse errors")
	}
	if _, ok := steerFormat("file:///c.steer", "// note\n"+misformattedSteerSrc); ok {
		t.Error("format accepted a program with // comments, which it would drop")
	}

	out, ok = steerFormat("file:///s.steer", "#!/usr/bin/env uta\n"+misformattedSteerSrc)
	if !ok || !strings.HasPrefix(out, "#!/usr/bin/env uta\n") {
		t.Errorf("shebang not preserved: ok=%v out=%q", ok, out)
	}
}

func TestSteerDiagnostics_CleanProgramAndParseError(t *testing.T) {
	clean := strings.Replace(brokenSteerSrc, "gret", "greet", 1)
	if diags := steerDiagnostics("file:///ok.steer", clean); len(diags) != 0 {
		t.Errorf("clean program produced diagnostics: %+v", diags)
	}
	if diags := steerDiagnostics("file:///garbage.steer", "not a steer program"); len(diags) != 1 {
		t.Errorf("parse error should yield exactly one diagnostic, got %+v", diags)
	}
}

// lspFrame / readLSPFrame mirror the base-protocol framing the lsp package
// speaks; kept local so the test exercises the wire, not the internals.
func lspFrame(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

func readLSPFrame(br *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			if length, err = strconv.Atoi(strings.TrimSpace(v)); err != nil {
				return nil, err
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("frame missing Content-Length")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(br, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// TestMissionLsp_DidOpenBrokenProgram drives the actual `uta mission lsp`
// command over an in-memory pipe: initialize, didOpen a broken program,
// and assert the published diagnostic's position and message.
func TestMissionLsp_DidOpenBrokenProgram(t *testing.T) {
	cmd := newMissionLspCmd()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	cmd.SetIn(inR)
	cmd.SetOut(outW)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{})

	done := make(chan error, 1)
	go func() {
		err := cmd.Execute()
		outW.Close()
		done <- err
	}()
	br := bufio.NewReader(outR)

	send := func(v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := io.WriteString(inW, lspFrame(string(b))); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	recv := func(v any) {
		t.Helper()
		payload, err := readLSPFrame(br)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if err := json.Unmarshal(payload, v); err != nil {
			t.Fatalf("unmarshal %s: %v", payload, err)
		}
	}

	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	var initResp struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	recv(&initResp)
	if initResp.ID != 1 || len(initResp.Result) == 0 {
		t.Fatalf("initialize response = %+v", initResp)
	}

	send(map[string]any{"jsonrpc": "2.0", "method": "initialized"})
	send(map[string]any{
		"jsonrpc": "2.0", "method": "textDocument/didOpen",
		"params": map[string]any{
			"textDocument": map[string]any{
				"uri": "file:///broken.steer", "languageId": "steer", "version": 1, "text": brokenSteerSrc,
			},
		},
	})
	var pub struct {
		Method string `json:"method"`
		Params struct {
			URI         string           `json:"uri"`
			Diagnostics []lsp.Diagnostic `json:"diagnostics"`
		} `json:"params"`
	}
	recv(&pub)
	if pub.Method != "textDocument/publishDiagnostics" || pub.Params.URI != "file:///broken.steer" {
		t.Fatalf("publish = %+v", pub)
	}
	if len(pub.Params.Diagnostics) == 0 {
		t.Fatal("no diagnostics published for a broken program")
	}
	d := pub.Params.Diagnostics[0]
	if d.Range.Start.Line != 4 || !strings.Contains(d.Message, `unknown function "gret"`) {
		t.Errorf("diagnostic = %+v", d)
	}

	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "shutdown"})
	var shutResp struct {
		ID int `json:"id"`
	}
	recv(&shutResp)
	if shutResp.ID != 2 {
		t.Fatalf("shutdown response id = %d", shutResp.ID)
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "exit"})
	if err := <-done; err != nil {
		t.Fatalf("mission lsp: %v", err)
	}
	inW.Close()
}

// TestMissionLsp_Formatting drives `uta mission lsp` over the wire: a
// misformatted mission comes back as one whole-document edit in fmt's
// canonical shape, a syntactically broken one answers null.
func TestMissionLsp_Formatting(t *testing.T) {
	cmd := newMissionLspCmd()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	cmd.SetIn(inR)
	cmd.SetOut(outW)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{})

	done := make(chan error, 1)
	go func() {
		err := cmd.Execute()
		outW.Close()
		done <- err
	}()
	br := bufio.NewReader(outR)

	send := func(v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := io.WriteString(inW, lspFrame(string(b))); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	recv := func(v any) {
		t.Helper()
		payload, err := readLSPFrame(br)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if err := json.Unmarshal(payload, v); err != nil {
			t.Fatalf("unmarshal %s: %v", payload, err)
		}
	}

	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	var initResp struct {
		Result struct {
			Capabilities struct {
				DocumentFormattingProvider bool `json:"documentFormattingProvider"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	recv(&initResp)
	if !initResp.Result.Capabilities.DocumentFormattingProvider {
		t.Fatal("initialize response does not advertise documentFormattingProvider")
	}

	send(map[string]any{
		"jsonrpc": "2.0", "method": "textDocument/didOpen",
		"params": map[string]any{
			"textDocument": map[string]any{
				"uri": "file:///m.steer", "languageId": "steer", "version": 1, "text": misformattedSteerSrc,
			},
		},
	})
	var pub struct {
		Method string `json:"method"`
	}
	recv(&pub) // the didOpen publishDiagnostics, discarded
	if pub.Method != "textDocument/publishDiagnostics" {
		t.Fatalf("after didOpen got %q, want publishDiagnostics", pub.Method)
	}

	send(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "textDocument/formatting",
		"params": map[string]any{
			"textDocument": map[string]any{"uri": "file:///m.steer"},
			"options":      map[string]any{"tabSize": 2, "insertSpaces": true},
		},
	})
	var fmtResp struct {
		ID     int            `json:"id"`
		Result []lsp.TextEdit `json:"result"`
	}
	recv(&fmtResp)
	if fmtResp.ID != 2 || len(fmtResp.Result) != 1 {
		t.Fatalf("formatting response = %+v, want one edit", fmtResp)
	}
	edit := fmtResp.Result[0]
	if want := canonicalSteer(t, misformattedSteerSrc); edit.NewText != want {
		t.Errorf("newText:\n%s\nwant:\n%s", edit.NewText, want)
	}
	// The edit must span the whole 6-line document (zero-based end 5:1,
	// after the closing brace).
	if edit.Range.Start != (lsp.Position{}) || edit.Range.End != (lsp.Position{Line: 5, Character: 1}) {
		t.Errorf("edit range = %+v", edit.Range)
	}

	// Break the document; formatting must now answer null.
	send(map[string]any{
		"jsonrpc": "2.0", "method": "textDocument/didChange",
		"params": map[string]any{
			"textDocument":   map[string]any{"uri": "file:///m.steer", "version": 2},
			"contentChanges": []map[string]any{{"text": "mission {"}},
		},
	})
	recv(&pub) // the didChange publishDiagnostics, discarded
	send(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "textDocument/formatting",
		"params": map[string]any{"textDocument": map[string]any{"uri": "file:///m.steer"}},
	})
	var nullResp struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	recv(&nullResp)
	if nullResp.ID != 3 || string(nullResp.Result) != "null" {
		t.Fatalf("formatting a broken program = %+v, want null result", nullResp)
	}

	send(map[string]any{"jsonrpc": "2.0", "method": "exit"})
	if err := <-done; err != nil {
		t.Fatalf("mission lsp: %v", err)
	}
	inW.Close()
}
