package steer

import (
	"fmt"
	"sort"
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
	c := &checker{prog: prog, lines: strings.Split(src, "\n"), refines: map[string]int{}, lensSets: map[string][]string{}}
	c.run()
	return c.diags
}

// CheckPolicy resolves the mission's `policy P` declaration against the
// names of the MissionProfiles actually installed. It is separate from
// Check because profile discovery reads the filesystem — the caller
// (uta mission check / run) loads the profiles and passes their names,
// keeping this package pure. Returns nil when the mission declares no
// policy or the name resolves; otherwise one hard Diag with a
// did-you-mean suggestion.
func CheckPolicy(prog *Program, src string, available []string) *Diag {
	if prog == nil || prog.Mission == nil || prog.Mission.Policy == nil {
		return nil
	}
	pol := prog.Mission.Policy
	for _, name := range available {
		if name == pol.Name {
			return nil
		}
	}
	c := &checker{prog: prog, lines: strings.Split(src, "\n")}
	c.errAt(pol.Pos, "unknown policy %q — no MissionProfile with that name (%s)%s",
		pol.Name, availableProfiles(available), suggest(pol.Name, available))
	return &c.diags[0]
}

// CheckCapRaises verifies every `with cap C from policy P` block against
// the installed MissionProfiles: P must exist and its allowed_tools must
// grant C, so an unbacked raise fails at `uta mission check`, not at run
// time. Like CheckPolicy this stays pure — callers load the profiles and
// pass name → allowed_tools, plus an optional match deciding whether a
// granted pattern covers the raised capability (the CLI passes the
// capability gate's pattern subsumption, so a grant of "Bash(git push *)"
// backs a raise of "Bash(git push origin main)"; nil means exact string
// equality). Returns every problem found — one Diag per bad raise — so a
// mission with several unbacked raises surfaces them all in one check run.
func CheckCapRaises(prog *Program, src string, grants map[string][]string, match func(grant, cap string) bool) []Diag {
	if prog == nil || prog.Mission == nil {
		return nil
	}
	if match == nil {
		match = func(grant, cap string) bool { return grant == cap }
	}
	names := make([]string, 0, len(grants))
	for n := range grants {
		names = append(names, n)
	}
	sort.Strings(names)
	c := &checker{prog: prog, lines: strings.Split(src, "\n")}
	for _, ws := range prog.Mission.CapRaises() {
		granted, ok := grants[ws.Policy]
		if !ok {
			c.errAt(ws.Pos, "with cap: unknown policy %q — no MissionProfile with that name (%s)%s",
				ws.Policy, availableProfiles(names), suggest(ws.Policy, names))
			continue
		}
		found := false
		for _, g := range granted {
			if match(g, ws.Cap) {
				found = true
				break
			}
		}
		if !found {
			c.errAt(ws.Pos, "policy %q does not grant capability %q — a raise can only borrow a capability its source policy actually allows%s",
				ws.Policy, ws.Cap, suggest(ws.Cap, granted))
		}
	}
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
	// refines is the checker's static prediction of each let binding's @k
	// refinement level (the runtime journal tag remains the source of
	// truth). Bindings share the mission's flat scope, so one map suffices.
	refines map[string]int
	// lensSets is refines' named twin: the standing lens set each binding
	// is statically guaranteed to carry (the runtime's `lenses` journal tag
	// remains the source of truth). Empty/absent means no specific lens is
	// guaranteed — only the count in refines.
	lensSets map[string][]string
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

// recordFieldTypes are the JSON-schema-able field types of v0.
var recordFieldTypes = map[string]bool{"Text": true, "Int": true, "Bool": true}

// retryErrorNames maps the typed failure names a `retry n on E` clause may
// name to whether re-evaluation is allowed. BudgetExceeded is recognized
// but not retryable: by the time it fires the budget is already spent, so
// a retry could only overspend further.
var retryErrorNames = map[string]bool{
	"SchemaMismatch": true,
	"JudgeRejected":  true,
	"BudgetExceeded": false,
}

func retryErrorNameList() []string {
	out := make([]string, 0, len(retryErrorNames))
	for n := range retryErrorNames {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// handlerErrorNames are the typed failures an `on E -> …` mission handler
// may name. Unlike retry, BudgetExceeded is the flagship case here — the
// handler compensates after the failure instead of re-running work, so it
// cannot overspend (any call it makes is still charged and re-trips the
// ceiling).
var handlerErrorNames = []string{"BudgetExceeded", "GateDenied", "GateTimedOut", "JudgeRejected", "SchemaMismatch"}

func (c *checker) run() {
	seenTypes := map[string]Pos{}
	for _, t := range c.prog.Types {
		if prev, dup := seenTypes[t.Name]; dup {
			c.errAt(t.Pos, "type %q already declared at line %d", t.Name, prev.Line)
		}
		seenTypes[t.Name] = t.Pos
		fields := map[string]bool{}
		for _, f := range t.Fields {
			if fields[f.Name] {
				c.errAt(f.Pos, "duplicate field %q in type %q", f.Name, t.Name)
			}
			fields[f.Name] = true
			if !recordFieldTypes[f.Type] {
				c.errAt(f.Pos, "field %q has type %q — record fields are Text, Int, or Bool in v0", f.Name, f.Type)
			}
		}
	}

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
	// Return types: Text (or [Text]) flows as-is; a declared record (or
	// [record]) is schema-validated at the agent boundary. Anything else
	// is a typo or an undeclared type.
	ret := strings.TrimSuffix(strings.TrimPrefix(fn.ReturnType, "["), "]")
	if ret != "Text" && c.prog.Type(ret) == nil {
		c.errAt(fn.Pos, "agent fn %q returns %q, which is not Text and not a declared type%s",
			fn.Name, fn.ReturnType, suggest(ret, typeNames(c.prog)))
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

	c.checkStmts(m, m.Stmts, lets, used, calledFns, &emitted)

	// Handlers share the mission's flat scope: compensation statements read
	// the lets the failed walk already bound. An emit here counts as the
	// mission emitting — the failure path is still a public result.
	seenHandlers := map[string]Pos{}
	for _, h := range m.Handlers {
		known := false
		for _, n := range handlerErrorNames {
			if n == h.ErrName {
				known = true
				break
			}
		}
		if !known {
			c.errAt(h.ErrPos, "unknown error name %q — on-handlers catch typed failures (%s)%s",
				h.ErrName, strings.Join(handlerErrorNames, ", "), suggest(h.ErrName, handlerErrorNames))
		}
		if prev, dup := seenHandlers[h.ErrName]; dup {
			c.errAt(h.Pos, "duplicate handler for %s (already declared at line %d)", h.ErrName, prev.Line)
		}
		seenHandlers[h.ErrName] = h.Pos
		c.checkStmts(m, h.Body, lets, used, calledFns, &emitted)
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

// checkStmts walks a statement list — the mission body or a nested block
// (`with cap`, `until dry`). Bindings share the mission's flat scope (a
// with block scopes authority, not names; an until body's bindings hold
// the loop's accumulated yield after it exits).
func (c *checker) checkStmts(m *Mission, stmts []Stmt, lets map[string]Pos, used, calledFns map[string]bool, emitted *bool) {
	for _, st := range stmts {
		switch s := st.(type) {
		case *LetStmt:
			c.checkExpr(s.Expr, lets, used, calledFns)
			if prev, dup := lets[s.Name]; dup {
				c.errAt(s.Pos, "binding %q already declared at line %d (bindings are immutable in v0)", s.Name, prev.Line)
			}
			lets[s.Name] = s.Pos
			c.refines[s.Name] = c.refinementOf(s.Expr)
			c.lensSets[s.Name] = c.lensSetOf(s.Expr)
		case *EmitStmt:
			c.checkExpr(s.Expr, lets, used, calledFns)
			*emitted = true
		case *GateStmt:
			c.checkExpr(s.Message, lets, used, calledFns)
			if s.Options != nil {
				if len(s.Options) == 0 {
					c.errAt(s.Pos, "gate choose needs at least one option (e.g. of [\"ship\", \"hold\"])")
				}
				seen := map[string]bool{}
				for _, opt := range s.Options {
					if opt == "" {
						c.errAt(s.Pos, "gate choose option must not be empty — an empty approval marker means \"no option chosen\"")
						continue
					}
					if seen[opt] {
						c.errAt(s.Pos, "duplicate option %q in gate choose", opt)
					}
					seen[opt] = true
				}
			}
		case *WithStmt:
			if m.Policy == nil {
				c.warnAt(s.Pos, "with cap raises a capability but mission %q binds no policy — without a `policy P` declaration there is no capability gate to raise", m.Name)
			}
			c.checkStmts(m, s.Body, lets, used, calledFns, emitted)
		case *UntilStmt:
			// The budget guard is mandatory: dryness alone cannot bound a
			// discovery loop whose rounds keep yielding, so the loop's other
			// exit — budget exhaustion — must actually be reachable.
			if m.Budget == nil || (m.Budget.Tokens == 0 && m.Budget.USDCents == 0) {
				c.errAt(s.Pos, "until dry(%d) has no budget guard — a discovery loop is only bounded by the mission's token or dollar ceiling (add e.g. `budget 50k tokens`)", s.N)
			}
			c.checkStmts(m, s.Body, lets, used, calledFns, emitted)
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
		// The loop variable is a raw item — always @0 with no lenses. It may
		// shadow a judged outer binding, so its refinement is scoped too.
		savedRefine, hadRefine := c.refines[x.Var]
		delete(c.refines, x.Var)
		savedLenses, hadLenses := c.lensSets[x.Var]
		delete(c.lensSets, x.Var)
		c.checkExpr(x.Body, child, used, calledFns)
		if hadRefine {
			c.refines[x.Var] = savedRefine
		}
		if hadLenses {
			c.lensSets[x.Var] = savedLenses
		}
	case *JudgeExpr:
		c.checkExpr(x.Value, lets, used, calledFns)
		if x.N != len(x.By) {
			c.errAt(x.Pos, "judge declares %d verifier(s) but `require %d of %d` — N must match the verifier count", len(x.By), x.K, x.N)
		}
		if x.K < 1 || x.K > x.N {
			c.errAt(x.Pos, "require %d of %d is unsatisfiable — K must be between 1 and N", x.K, x.N)
		}
		for _, by := range x.By {
			fn := c.prog.Agent(by.Name)
			if fn == nil {
				c.errAt(by.Pos, "unknown function %q%s", by.Name, suggest(by.Name, agentNames(c.prog)))
				continue
			}
			calledFns[by.Name] = true
			if len(fn.Params) < 1 {
				c.errAt(by.Pos, "judge verifier %q needs at least one parameter — the runtime appends the judged value as the last argument", fn.Name)
			} else if len(by.Args) != len(fn.Params)-1 {
				c.errAt(by.Pos, "judge verifier %s takes %d argument(s) here (its last parameter %q receives the judged value), got %d",
					fn.Name, len(fn.Params)-1, fn.Params[len(fn.Params)-1].Name, len(by.Args))
			}
			if _, _, structured := c.prog.SchemaFor(fn); structured {
				c.errAt(by.Pos, "judge verifier %q must return Text — its verdict contract (STANDS/REFUTED) conflicts with a record schema", fn.Name)
			}
			for i, a := range by.Args {
				c.checkExpr(a, lets, used, calledFns)
				if i < len(fn.Params)-1 {
					c.checkArgRefinement(fn, fn.Params[i], a)
				}
			}
			// The runtime appends the judged value as the last argument, so
			// a declared @k on the last parameter constrains the value
			// entering the judge — same contract as an explicit argument.
			if len(fn.Params) > 0 {
				c.checkArgRefinement(fn, fn.Params[len(fn.Params)-1], x.Value)
			}
		}
	case *RetryExpr:
		retryable, known := retryErrorNames[x.ErrName]
		switch {
		case !known:
			c.errAt(x.ErrPos, "unknown error name %q — retry catches typed failures (%s)%s",
				x.ErrName, strings.Join(retryErrorNameList(), ", "), suggest(x.ErrName, retryErrorNameList()))
		case !retryable:
			c.errAt(x.ErrPos, "retry on BudgetExceeded is not allowed — the budget is already spent when it fires, so a retry could only overspend further")
		}
		c.checkExpr(x.Expr, lets, used, calledFns)
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
		for i, a := range x.Args {
			c.checkExpr(a, lets, used, calledFns)
			if i < len(fn.Params) {
				c.checkArgRefinement(fn, fn.Params[i], a)
			}
		}
	}
}

// refinementOf predicts the @k level a value will carry at run time — the
// static twin of the engine's judgedLevel: K for `judge ... require K of N`
// (through retry wrappers, since a value only arrives if some attempt's
// judge passed, and through par-for bodies, since the join only arrives
// when every branch's judge passed), a let binding's inferred level for an
// identifier, and 0 for everything else.
func (c *checker) refinementOf(e Expr) int {
	for {
		switch x := e.(type) {
		case *JudgeExpr:
			return x.K
		case *RetryExpr:
			e = x.Expr
		case *ParForExpr:
			// The loop variable is a raw item (@0) inside the body — scope
			// its refinement out the way checkExpr does before descending,
			// so a body that just echoes the variable stays unjudged even
			// when it shadows a judged outer binding.
			saved, had := c.refines[x.Var]
			delete(c.refines, x.Var)
			k := c.refinementOf(x.Body)
			if had {
				c.refines[x.Var] = saved
			}
			return k
		case *Ident:
			return c.refines[x.Name]
		default:
			return 0
		}
	}
}

// lensSetOf predicts the standing lens set a value will carry at run
// time — the static twin of the `lenses` tag the runtime journals on
// judge-passed values. A judge guarantees its lens set only when K == N:
// every verifier must have stood for the value to arrive, so the set is
// exactly {LensName(by)}. With K < N any K-subset may have stood, so no
// specific lens is certain — only the count — and the prediction is
// empty. Propagates through retry wrappers and let bindings the way
// refinementOf does. Names come from LensName, the same function the
// runtime journals with, so the prediction cannot drift from the truth.
func (c *checker) lensSetOf(e Expr) []string {
	for {
		switch x := e.(type) {
		case *JudgeExpr:
			if x.K != x.N {
				return nil
			}
			set := make(map[string]bool, len(x.By))
			for _, by := range x.By {
				set[LensName(by)] = true
			}
			out := make([]string, 0, len(set))
			for l := range set {
				out = append(out, l)
			}
			sort.Strings(out)
			return out
		case *RetryExpr:
			e = x.Expr
		case *ParForExpr:
			saved, had := c.lensSets[x.Var]
			delete(c.lensSets, x.Var)
			ls := c.lensSetOf(x.Body)
			if had {
				c.lensSets[x.Var] = saved
			}
			return ls
		case *Ident:
			return c.lensSets[x.Name]
		default:
			return nil
		}
	}
}

// checkArgRefinement enforces the @k subtype chain @2 <: @1 <: @0 at a
// call site: an argument may carry more judgment than the parameter
// demands, never less. Passing an under-judged value is a check-time
// error with a fix-it hint naming the missing judge step. When the
// parameter names its lenses (`@ {Security, Repro}`), the count is only
// the necessary condition — the named half is checked once it holds.
func (c *checker) checkArgRefinement(fn *AgentFn, p Param, arg Expr) {
	if p.Refine == 0 {
		return
	}
	got := c.refinementOf(arg)
	if got >= p.Refine {
		c.checkArgLenses(fn, p, arg)
		return
	}
	what, val := "the argument", "<value>"
	if id, ok := arg.(*Ident); ok {
		what, val = fmt.Sprintf("%q", id.Name), id.Name
	}
	level := fmt.Sprintf("@%d", got)
	if got == 0 {
		level += " (unjudged)"
	}
	c.errAt(arg.exprPos(),
		"%s expects %s: %s but %s is %s — add the missing judge step first: `let %s_judged = judge %s by <verifier>, ... require %d of %d` and pass %s_judged",
		fn.Name, p.Name, p.TypeString(), what, level, val, val, p.Refine, p.Refine, val)
}

// checkArgLenses enforces named-lens subsumption at a call site: every
// lens the parameter demands must be in the argument's statically
// guaranteed standing set. Runs only after the count projection passed,
// so an empty inferred set here means the value cleared a judge with
// K < N — enough count, but no specific lens guaranteed to have stood.
func (c *checker) checkArgLenses(fn *AgentFn, p Param, arg Expr) {
	if len(p.Lenses) == 0 {
		return
	}
	got := c.lensSetOf(arg)
	var missing []string
	for _, want := range p.Lenses {
		found := false
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, want)
		}
	}
	if len(missing) == 0 {
		return
	}
	what := "the argument"
	if id, ok := arg.(*Ident); ok {
		what = fmt.Sprintf("%q", id.Name)
	}
	if len(got) == 0 {
		c.errAt(arg.exprPos(),
			"%s expects %s: %s but %s guarantees no named lens — its judge requires K of N with K < N, so which verifiers stood is unknown until run time; a lens set is only guaranteed by `require N of N` (every verifier must stand)",
			fn.Name, p.Name, p.TypeString(), what)
		return
	}
	c.errAt(arg.exprPos(),
		"%s expects %s: %s but %s carries lens set {%s} — missing {%s}; add a verifier per missing lens to the judge (a lens is the verifier's first string-literal argument, else its fn name) and require all of them",
		fn.Name, p.Name, p.TypeString(), what, strings.Join(got, ", "), strings.Join(missing, ", "))
}

func paramNames(fn *AgentFn) []string {
	out := make([]string, len(fn.Params))
	for i, p := range fn.Params {
		out[i] = p.Name
	}
	return out
}

func typeNames(prog *Program) []string {
	out := make([]string, 0, len(prog.Types)+1)
	for _, t := range prog.Types {
		out = append(out, t.Name)
	}
	return append(out, "Text")
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

// availableProfiles renders the installed-MissionProfile list for a
// policy diagnostic. An empty list is exactly when the user most needs
// direction, so instead of a bare "available: " it says where profiles
// come from.
func availableProfiles(names []string) string {
	if len(names) == 0 {
		return "no MissionProfiles installed — create one under ~/.uta/profiles/"
	}
	return "available: " + strings.Join(names, ", ")
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
