package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/steer"
)

func newMissionFmtCmd() *cobra.Command {
	var write, check bool
	cmd := &cobra.Command{
		Use:   "fmt <file.steer>",
		Short: "format a steer program into the one canonical shape",
		Long: `Formats a .steer program in canonical style: two-space indent, one blank
line between declarations, agent clauses in worker/costs/prompt order,
normalized budget lists and worker clauses. By default the changes print
as a unified diff; --write rewrites the file in place; --check prints the
file name and exits 1 if it is not canonically formatted, for CI. A
leading #! shebang line is preserved.

v0 limitation: the formatter prints from the parsed AST, which does not
carry // comments — rather than silently drop them, fmt refuses a file
that contains comments.`,
		Example: `  uta mission fmt hello.steer            # show what would change, as a diff
  uta mission fmt hello.steer --write    # rewrite the file
  uta mission fmt hello.steer --check    # exit 1 if not formatted (CI)`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSteerFiles,
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
				return exitWith(2)
			}
			src := string(data)
			prog, pd := steer.Parse(filepath.Base(args[0]), src)
			if pd != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), pd.Render())
				return exitWith(2)
			}
			if steer.HasComments(src) {
				fmt.Fprintf(cmd.ErrOrStderr(), "error: %s contains // comments, which the v0 formatter would drop (the AST does not carry them yet) — not formatting\n", args[0])
				return exitWith(2)
			}
			out := steer.Format(prog)
			// The AST has no shebang either, but an executable .steer must
			// stay executable: carry the #! line over verbatim.
			if strings.HasPrefix(src, "#!") {
				line := src
				if nl := strings.IndexByte(src, '\n'); nl >= 0 {
					line = src[:nl]
				}
				out = strings.TrimRight(line, "\r") + "\n" + out
			}
			if out == src {
				return nil
			}
			if check {
				fmt.Fprintln(cmd.OutOrStdout(), args[0])
				return exitWith(1)
			}
			if write {
				mode := fs.FileMode(0o644)
				if info, serr := os.Stat(args[0]); serr == nil {
					mode = info.Mode().Perm()
				}
				if werr := os.WriteFile(args[0], []byte(out), mode); werr != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "error:", werr)
					return exitWith(2)
				}
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), unifiedDiff(args[0], src, out))
			return nil
		},
	}
	cmd.Flags().BoolVar(&write, "write", false, "rewrite the file instead of printing a diff")
	cmd.Flags().BoolVar(&check, "check", false, "print the file name and exit 1 if it needs formatting")
	cmd.MarkFlagsMutuallyExclusive("write", "check")
	return cmd
}

// unifiedDiff renders a minimal unified diff (3 lines of context) between
// the original and formatted source. Small inputs, so the plain LCS table
// is fine.
func unifiedDiff(path, before, after string) string {
	ops := diffOps(diffSplit(before), diffSplit(after))
	changed := false
	for _, op := range ops {
		if op[0] != ' ' {
			changed = true
			break
		}
	}
	if !changed {
		return ""
	}

	// Line numbers on each side at every op index, for hunk headers.
	aAt := make([]int, len(ops)+1)
	bAt := make([]int, len(ops)+1)
	al, bl := 1, 1
	for i, op := range ops {
		aAt[i], bAt[i] = al, bl
		switch op[0] {
		case ' ':
			al++
			bl++
		case '-':
			al++
		case '+':
			bl++
		}
	}
	aAt[len(ops)], bAt[len(ops)] = al, bl

	// Hunks: each changed op ± context, overlapping ranges merged.
	const ctx = 3
	var hunks [][2]int
	for idx, op := range ops {
		if op[0] == ' ' {
			continue
		}
		lo, hi := idx-ctx, idx+ctx+1
		if lo < 0 {
			lo = 0
		}
		if hi > len(ops) {
			hi = len(ops)
		}
		if n := len(hunks); n > 0 && lo <= hunks[n-1][1] {
			if hi > hunks[n-1][1] {
				hunks[n-1][1] = hi
			}
		} else {
			hunks = append(hunks, [2]int{lo, hi})
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s (formatted)\n", path, path)
	for _, h := range hunks {
		aCount, bCount := 0, 0
		for _, op := range ops[h[0]:h[1]] {
			if op[0] != '+' {
				aCount++
			}
			if op[0] != '-' {
				bCount++
			}
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", aAt[h[0]], aCount, bAt[h[0]], bCount)
		for _, op := range ops[h[0]:h[1]] {
			b.WriteString(op)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// diffOps computes a line-level diff via LCS; each op is a line prefixed
// with ' ' (common), '-' (only in a), or '+' (only in b).
func diffOps(a, b []string) []string {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				lcs[i][j] = lcs[i+1][j+1] + 1
			case lcs[i+1][j] >= lcs[i][j+1]:
				lcs[i][j] = lcs[i+1][j]
			default:
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	ops := make([]string, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, " "+a[i])
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, "-"+a[i])
			i++
		default:
			ops = append(ops, "+"+b[j])
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, "-"+a[i])
	}
	for ; j < m; j++ {
		ops = append(ops, "+"+b[j])
	}
	return ops
}

// diffSplit splits source into lines for diffing, dropping the phantom
// empty element a trailing newline would produce.
func diffSplit(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
