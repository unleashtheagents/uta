package steer

import (
	"fmt"
	"strings"
)

// Check runs the v0 static checks over a parsed program. It returns every
// diagnostic it finds (errors and warnings); callers refuse to execute when
// any non-warning diag is present. src is the original source, used for
// excerpt lines in diagnostics.
//
// The v0 rules, in the spirit of the RFC:
//   - exactly one mission per file
//   - a mission must declare a budget with a token or dollar ceiling —
//     budget is the memory-safety of the agent era, so an unbounded
//     mission refuses to compile
//   - every call names a declared agent fn with matching arity
//   - every ${slot} in a prompt names a parameter of its agent fn
//   - every agent fn has a prompt block (the only place natural language
//     lives)
//   - let bindings are unique and identifiers resolve
//   - warnings: unused lets, unused agent fns, missing emit
func Check(prog *Program, src string) []Diag {
	c := &checker{prog: prog, lines: strings.Split(src, "\n")}
	c.run()
	return c.diags
}

// HasErrors reports whether any diagnostic is a hard error.
func HasErrors(diags []Diag) bool {
	for _, d := range diags {
		if !d.Warning {
			return true
		}
	}
	return false
}

type checker struct {
	prog  *Program
	lines []string
	diags []Diag
}

func (c *checker) errAt(p Pos, format string, args ...any) {
	c.diags = append(c.diags, Diag{File: c.prog.File, Pos: p, Msg: fmt.Sprintf(format, args...), SrcLine: c.srcLine(p.Line)})
}

func (c *checker) warnAt(p Pos, format string, args ...any) {
	c.diags = append(c.diags, Diag{File: c.prog.File, Pos: p, Msg: fmt.Sprintf(format, args...), SrcLine: c.srcLine(p.Line), Warning: true})
}

func (c *checker) srcLine(n int) string {
	if n >= 1 && n <= len(c.lines) {
		return strings.TrimRight(c.lines[n-1], "\r")
	}
	return ""
}

func (c *checker) run() {
	seen := map[string]Pos{}
	for _, fn := range c.prog.Agents {
		if prev, dup := seen[fn.Name]; dup {
			c.errAt(fn.Pos, "agent fn %q already declared at line %d", fn.Name, prev.Line)
		}
		seen[fn.Name] = fn.Pos
		c.checkAgentFn(fn)
	}

	if c.prog.Mission == nil {
		c.errAt(Pos{1, 1}, "no mission declared — a .steer file needs exactly one mission block")
		return
	}
	c.checkMission(c.prog.Mission)
}

func (c *checker) checkAgentFn(fn *AgentFn) {
	if fn.Prompt.Text == "" {
		c.errAt(fn.Pos, "agent fn %q has no prompt block — the body of an agent fn is a goal, and the prompt is where it lives", fn.Name)
	}
	params := map[string]bool{}
	for _, p := range fn.Params {
		if params[p.Name] {
			c.errAt(p.Pos, "duplicate parameter %q in agent fn %q", p.Name, fn.Name)
		}
		params[p.Name] = true
	}
	for _, s := range fn.Prompt.Slots {
		if !params[s.Name] {
			c.errAt(s.Pos, "prompt of agent fn %q interpolates ${%s}, which is not a parameter%s",
				fn.Name, s.Name, suggest(s.Name, paramNames(fn)))
		}
	}
}

func (c *checker) checkMission(m *Mission) {
	if m.Budget == nil {
		c.errAt(m.Pos, "mission %q declares no budget — steer refuses to run an unbounded mission (add e.g. `budget 50k tokens, 10min`)", m.Name)
	} else if m.Budget.Tokens == 0 && m.Budget.USDCents == 0 {
		c.errAt(m.Budget.Pos, "mission %q has a budget with neither a token nor a dollar ceiling — a duration alone does not bound spend", m.Name)
	}

	lets := map[string]Pos{}
	used := map[string]bool{}
	calledFns := map[string]bool{}
	emitted := false

	for _, st := range m.Stmts {
		switch s := st.(type) {
		case *LetStmt:
			c.checkExpr(s.Expr, lets, used, calledFns)
			if prev, dup := lets[s.Name]; dup {
				c.errAt(s.Pos, "binding %q already declared at line %d (bindings are immutable in v0)", s.Name, prev.Line)
			}
			lets[s.Name] = s.Pos
		case *EmitStmt:
			c.checkExpr(s.Expr, lets, used, calledFns)
			emitted = true
		}
	}

	if !emitted {
		c.warnAt(m.Pos, "mission %q never emits — it will run its calls and produce no public result", m.Name)
	}
	for name, pos := range lets {
		if !used[name] {
			c.warnAt(pos, "binding %q is never used", name)
		}
	}
	for _, fn := range c.prog.Agents {
		if !calledFns[fn.Name] {
			c.warnAt(fn.Pos, "agent fn %q is declared but never called", fn.Name)
		}
	}
}

func (c *checker) checkExpr(e Expr, lets map[string]Pos, used, calledFns map[string]bool) {
	switch x := e.(type) {
	case *StringLit:
	case *Ident:
		if _, ok := lets[x.Name]; !ok {
			c.errAt(x.Pos, "unknown name %q%s", x.Name, suggest(x.Name, keys(lets)))
		}
		used[x.Name] = true
	case *ParForExpr:
		for _, it := range x.Items {
			c.checkExpr(it, lets, used, calledFns)
		}
		// The loop variable is visible only inside the body.
		child := make(map[string]Pos, len(lets)+1)
		for k, v := range lets {
			child[k] = v
		}
		child[x.Var] = x.Pos
		c.checkExpr(x.Body, child, used, calledFns)
	case *CallExpr:
		fn := c.prog.Agent(x.Name)
		if fn == nil {
			c.errAt(x.Pos, "unknown function %q%s", x.Name, suggest(x.Name, agentNames(c.prog)))
			return
		}
		calledFns[x.Name] = true
		if len(x.Args) != len(fn.Params) {
			c.errAt(x.Pos, "%s takes %d argument(s) (%s), got %d",
				fn.Name, len(fn.Params), strings.Join(paramNames(fn), ", "), len(x.Args))
		}
		for _, a := range x.Args {
			c.checkExpr(a, lets, used, calledFns)
		}
	}
}

func paramNames(fn *AgentFn) []string {
	out := make([]string, len(fn.Params))
	for i, p := range fn.Params {
		out[i] = p.Name
	}
	return out
}

func agentNames(prog *Program) []string {
	out := make([]string, len(prog.Agents))
	for i, a := range prog.Agents {
		out[i] = a.Name
	}
	return out
}

func keys(m map[string]Pos) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// suggest returns a `(did you mean "x"?)` suffix when a candidate is
// within edit distance 2 of the unknown name.
func suggest(name string, candidates []string) string {
	best, bestDist := "", 3
	for _, c := range candidates {
		if d := editDistance(name, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" (did you mean %q?)", best)
}

func editDistance(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
