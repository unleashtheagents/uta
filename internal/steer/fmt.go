package steer

import (
	"fmt"
	"strings"
	"time"
)

// Format prints a parsed program in the one canonical steer shape:
// declarations separated by a single blank line (types, then agent fns,
// then the mission), two-space indent, agent clauses in worker / costs /
// prompt order, and normalized budget lists (tokens, dollars, deadline).
// Formatting is idempotent — parsing the output and formatting again
// reproduces it byte for byte.
//
// Comments are not part of the AST, so formatting a commented source
// drops them; callers that rewrite files (uta mission fmt) refuse such
// input via HasComments until the AST carries comments.
func Format(prog *Program) string {
	var secs []string
	for _, t := range prog.Types {
		secs = append(secs, formatTypeDecl(t))
	}
	for _, a := range prog.Agents {
		secs = append(secs, formatAgentFn(a))
	}
	if prog.Mission != nil {
		secs = append(secs, formatMission(prog.Mission))
	}
	if len(secs) == 0 {
		return ""
	}
	return strings.Join(secs, "\n\n") + "\n"
}

// HasComments reports whether the source contains any // comment outside
// strings and prompt blocks. A leading #! shebang line does not count —
// it survives formatting (the CLI carries it over), comments would not.
func HasComments(src string) bool {
	i := 0
	if strings.HasPrefix(src, "#!") {
		nl := strings.IndexByte(src, '\n')
		if nl < 0 {
			return false
		}
		i = nl + 1
	}
	for i < len(src) {
		switch {
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '/':
			return true
		case strings.HasPrefix(src[i:], `"""`):
			end := strings.Index(src[i+3:], `"""`)
			if end < 0 {
				return false // unterminated; the parser reports it
			}
			i += 3 + end + 3
		case src[i] == '"':
			i++
			for i < len(src) && src[i] != '"' && src[i] != '\n' {
				if src[i] == '\\' {
					i++
				}
				i++
			}
			if i < len(src) {
				i++
			}
		default:
			i++
		}
	}
	return false
}

func formatTypeDecl(t *RecordType) string {
	var b strings.Builder
	fmt.Fprintf(&b, "type %s {\n", t.Name)
	for _, f := range t.Fields {
		fmt.Fprintf(&b, "  %s: %s\n", f.Name, f.Type)
	}
	b.WriteString("}")
	return b.String()
}

func formatAgentFn(a *AgentFn) string {
	var b strings.Builder
	b.WriteString("agent fn " + a.Name + "(")
	for i, p := range a.Params {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p.Name + ": " + p.TypeString())
	}
	b.WriteString(") -> " + a.ReturnType)
	if len(a.Workers) == 1 {
		b.WriteString("\n  worker " + a.Workers[0])
	} else if len(a.Workers) > 1 {
		b.WriteString("\n  worker any(" + strings.Join(a.Workers, ", ") + ")")
	}
	if a.CostTokens > 0 {
		b.WriteString("\n  costs <= " + formatTokenCount(a.CostTokens) + " tokens")
	}
	if a.Prompt.Text != "" {
		b.WriteString("\n  prompt \"\"\"\n")
		for _, ln := range strings.Split(a.Prompt.Text, "\n") {
			if ln == "" {
				b.WriteString("\n")
			} else {
				b.WriteString("  " + ln + "\n")
			}
		}
		b.WriteString("  \"\"\"")
	}
	return b.String()
}

func formatMission(m *Mission) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mission %s {\n", m.Name)
	decls := false
	if m.Policy != nil {
		fmt.Fprintf(&b, "  policy %s\n", m.Policy.Name)
		decls = true
	}
	if m.Budget != nil {
		fmt.Fprintf(&b, "  budget %s\n", formatBudgetDecl(m.Budget))
		decls = true
	}
	if decls && len(m.Stmts) > 0 {
		b.WriteString("\n")
	}
	for _, st := range m.Stmts {
		writeStmt(&b, st, "  ")
	}
	if len(m.Handlers) > 0 && (decls || len(m.Stmts) > 0) {
		b.WriteString("\n")
	}
	for _, h := range m.Handlers {
		b.WriteString("  on " + h.ErrName + " ->")
		for i, st := range h.Body {
			if i > 0 {
				b.WriteString(";")
			}
			b.WriteString(" ")
			writeStmtInline(&b, st, "  ")
		}
		b.WriteString("\n")
	}
	b.WriteString("}")
	return b.String()
}

// formatBudgetDecl normalizes the item order to tokens, dollars, deadline.
// An all-zero declaration (every dimension written as 0) still needs one
// item to reparse; "0 tokens" keeps it faithful.
func formatBudgetDecl(bd *BudgetDecl) string {
	var parts []string
	if bd.Tokens > 0 {
		parts = append(parts, formatTokenCount(bd.Tokens)+" tokens")
	}
	if bd.USDCents > 0 {
		if bd.USDCents%100 == 0 {
			parts = append(parts, fmt.Sprintf("$%d", bd.USDCents/100))
		} else {
			parts = append(parts, fmt.Sprintf("$%d.%02d", bd.USDCents/100, bd.USDCents%100))
		}
	}
	if bd.Deadline > 0 {
		parts = append(parts, formatDeadline(bd.Deadline))
	}
	if len(parts) == 0 {
		return "0 tokens"
	}
	return strings.Join(parts, ", ")
}

func formatTokenCount(n int64) string {
	if n >= 1000 && n%1000 == 0 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%d", n)
}

// formatDeadline picks the largest unit the grammar offers that divides
// the value evenly; the parser only builds whole-unit durations, so one
// of these always fits.
func formatDeadline(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dmin", d/time.Minute)
	default:
		return fmt.Sprintf("%ds", d/time.Second)
	}
}

// writeStmt writes one statement as a full line (or block) at the given
// indent.
func writeStmt(b *strings.Builder, st Stmt, indent string) {
	b.WriteString(indent)
	writeStmtInline(b, st, indent)
	b.WriteString("\n")
}

// writeStmtInline writes a statement without its leading indent or
// trailing newline — shared by block bodies and the single-line
// on-handler form. indent is the statement's own indent, used for
// continuation lines of multi-line expressions and nested blocks.
func writeStmtInline(b *strings.Builder, st Stmt, indent string) {
	switch s := st.(type) {
	case *LetStmt:
		b.WriteString("let " + s.Name + " = ")
		writeExpr(b, s.Expr, indent)
	case *EmitStmt:
		b.WriteString("emit ")
		writeExpr(b, s.Expr, indent)
	case *GateStmt:
		if s.Options != nil {
			b.WriteString("gate choose ")
			writeExpr(b, s.Message, indent)
			b.WriteString(" of [")
			for i, opt := range s.Options {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(quoteSteerString(opt))
			}
			b.WriteString("]")
		} else {
			b.WriteString("gate human ")
			writeExpr(b, s.Message, indent)
		}
		if s.Within > 0 {
			b.WriteString(" within " + formatDeadline(s.Within))
		}
	case *HaltStmt:
		b.WriteString("halt")
	case *WithStmt:
		b.WriteString("with cap " + quoteSteerString(s.Cap) + " from policy " + s.Policy + " {\n")
		for _, inner := range s.Body {
			writeStmt(b, inner, indent+"  ")
		}
		b.WriteString(indent + "}")
	case *UntilStmt:
		fmt.Fprintf(b, "until dry(%d) {\n", s.N)
		for _, inner := range s.Body {
			writeStmt(b, inner, indent+"  ")
		}
		b.WriteString(indent + "}")
	}
}

// writeExpr writes an expression. Calls, idents, and strings stay on one
// line; par for and judge always take their canonical multi-line shape.
func writeExpr(b *strings.Builder, e Expr, indent string) {
	switch x := e.(type) {
	case *StringLit:
		b.WriteString(quoteSteerString(x.Value))
	case *Ident:
		b.WriteString(x.Name)
	case *CallExpr:
		b.WriteString(x.Name + "(")
		for i, a := range x.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			writeExpr(b, a, indent)
		}
		b.WriteString(")")
	case *ParForExpr:
		b.WriteString("par for " + x.Var + " in [")
		for i, it := range x.Items {
			if i > 0 {
				b.WriteString(", ")
			}
			writeExpr(b, it, indent)
		}
		b.WriteString("] {\n" + indent + "  ")
		writeExpr(b, x.Body, indent+"  ")
		b.WriteString("\n" + indent + "}")
	case *JudgeExpr:
		b.WriteString("judge ")
		writeExpr(b, x.Value, indent)
		b.WriteString("\n" + indent + "  by ")
		for i, by := range x.By {
			if i > 0 {
				b.WriteString(", ")
			}
			writeExpr(b, by, indent+"  ")
		}
		fmt.Fprintf(b, "\n%s  require %d of %d", indent, x.K, x.N)
	case *RetryExpr:
		writeExpr(b, x.Expr, indent)
		fmt.Fprintf(b, " retry %d on %s", x.N, x.ErrName)
	}
}

// quoteSteerString renders a value as a steer string literal, escaping
// exactly what the lexer unescapes.
func quoteSteerString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
