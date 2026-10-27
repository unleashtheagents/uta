package cli

import (
	"path"
	"strings"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/lsp"
	"github.com/unleashtheagents/uta/internal/steer"
	"github.com/unleashtheagents/uta/internal/version"
)

func newMissionLspCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lsp",
		Short: "serve steer diagnostics to an editor (LSP over stdio)",
		Long: `Speaks the Language Server Protocol base protocol (JSON-RPC 2.0 with
Content-Length framing) on stdin/stdout. The method surface is the
LSP-lite minimum for push diagnostics plus formatting:
initialize/initialized, textDocument/didOpen, didChange (full sync),
didClose, textDocument/formatting, shutdown/exit. Every open and every
edit re-runs the same parse + static checks as 'uta mission check' and
pushes textDocument/publishDiagnostics — no tokens spent.
textDocument/formatting answers with the same canonical shape as
'uta mission fmt', as one whole-document edit — or null when the
program has parse errors or // comments the v0 formatter would drop,
the same inputs fmt refuses.

Unlike 'uta mission check', the 'policy P' profile lookup is skipped:
it reads the filesystem, and per-keystroke diagnostics should not.

Editor setup is one stanza: register "uta mission lsp" as the language
server for *.steer files.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			srv := lsp.NewServer("uta-steer", version.Version, steerDiagnostics)
			srv.SetFormatter(steerFormat)
			return srv.Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

// steerDiagnostics is the DiagnoseFunc behind `uta mission lsp`: the same
// parse → check pipeline as `uta mission check`, positions shifted to the
// protocol's zero-based lines and columns.
func steerDiagnostics(uri, text string) []lsp.Diagnostic {
	// URIs use '/' regardless of platform, so path (not filepath) gives
	// the bare file name steer diagnostics carry.
	prog, pd := steer.Parse(path.Base(uri), text)
	if pd != nil {
		return []lsp.Diagnostic{steerDiagToLSP(*pd)}
	}
	diags := steer.Check(prog, text)
	out := make([]lsp.Diagnostic, 0, len(diags))
	for _, d := range diags {
		out = append(out, steerDiagToLSP(d))
	}
	return out
}

// steerFormat is the FormatFunc behind `uta mission lsp`: the same
// canonical print as `uta mission fmt`, refusing the same inputs — a
// program with parse errors, or one with // comments the v0 formatter
// would drop. Like fmt, a leading #! shebang line is carried over.
func steerFormat(uri, text string) (string, bool) {
	prog, pd := steer.Parse(path.Base(uri), text)
	if pd != nil || steer.HasComments(text) {
		return "", false
	}
	out := steer.Format(prog)
	if strings.HasPrefix(text, "#!") {
		line := text
		if nl := strings.IndexByte(text, '\n'); nl >= 0 {
			line = text[:nl]
		}
		out = strings.TrimRight(line, "\r") + "\n" + out
	}
	return out, true
}

func steerDiagToLSP(d steer.Diag) lsp.Diagnostic {
	sev := lsp.SeverityError
	if d.Warning {
		sev = lsp.SeverityWarning
	}
	// steer positions are 1-based; a zero-width range at the offending
	// column is enough for editors to place the squiggle.
	pos := lsp.Position{Line: max(d.Pos.Line-1, 0), Character: max(d.Pos.Col-1, 0)}
	return lsp.Diagnostic{
		Range:    lsp.Range{Start: pos, End: pos},
		Severity: sev,
		Source:   "steer",
		Message:  d.Msg,
	}
}
