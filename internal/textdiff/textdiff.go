// Package textdiff produces unified diffs for small text files without
// shelling out. It backs the diff shown when an agent edits a file, the
// shell's /diff view and the file-checkpoint undo preview.
package textdiff

import (
	"fmt"
	"strings"
)

// Unified returns a unified diff between a and b, labelled with name. It
// returns "" when the inputs are identical. Context is three lines.
func Unified(name, a, b string) string {
	if a == b {
		return ""
	}
	al := splitLines(a)
	bl := splitLines(b)
	ops := diffOps(al, bl)

	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", name, name)
	const ctx = 3
	// Group ops into hunks separated by more than 2*ctx equal lines.
	type hunk struct{ start, end int }
	var hunks []hunk
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := i
		end := i
		for j := i; j < len(ops); j++ {
			if ops[j].kind != ' ' {
				end = j
				continue
			}
			// Look ahead: is there another change within 2*ctx equal lines?
			k := j
			for k < len(ops) && ops[k].kind == ' ' && k-j <= 2*ctx {
				k++
			}
			if k < len(ops) && ops[k].kind != ' ' && k-j <= 2*ctx {
				j = k - 1
				continue
			}
			break
		}
		hunks = append(hunks, hunk{start, end})
		i = end + 1
	}
	for _, h := range hunks {
		s := h.start - ctx
		if s < 0 {
			s = 0
		}
		e := h.end + ctx
		if e > len(ops)-1 {
			e = len(ops) - 1
		}
		aStart, bStart := ops[s].ai+1, ops[s].bi+1
		var aCount, bCount int
		var body strings.Builder
		for _, op := range ops[s : e+1] {
			switch op.kind {
			case ' ':
				aCount++
				bCount++
			case '-':
				aCount++
			case '+':
				bCount++
			}
			body.WriteByte(byte(op.kind))
			body.WriteString(op.text)
			body.WriteByte('\n')
		}
		if aCount == 0 {
			aStart = ops[s].ai
		}
		if bCount == 0 {
			bStart = ops[s].bi
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart, aCount, bStart, bCount)
		out.WriteString(body.String())
	}
	return out.String()
}

// Stat returns the number of added and removed lines between a and b.
func Stat(a, b string) (added, removed int) {
	for _, op := range diffOps(splitLines(a), splitLines(b)) {
		switch op.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return added, removed
}

type op struct {
	kind   rune // ' ', '-', '+'
	text   string
	ai, bi int // index into a / b at the time the op was emitted
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// diffOps computes an LCS-based line diff. It is O(n*m) in memory, so very
// large inputs fall back to a whole-file replacement.
func diffOps(a, b []string) []op {
	n, m := len(a), len(b)
	if n*m > 4_000_000 {
		var ops []op
		for i, l := range a {
			ops = append(ops, op{'-', l, i, 0})
		}
		for j, l := range b {
			ops = append(ops, op{'+', l, n, j})
		}
		return ops
	}
	// lcs[i][j] = length of LCS of a[i:], b[j:]
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i], i, j})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{'-', a[i], i, j})
			i++
		default:
			ops = append(ops, op{'+', b[j], i, j})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i], i, j})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j], i, j})
	}
	return ops
}
