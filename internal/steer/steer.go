// Package steer implements the v0 interpreter front-end for the steer
// language — the programming layer sketched in Paper № 02 on top of the
// uta control verbs ("steer is to the uta verbs what C is to assembly").
//
// This package is pure: lexer, parser, AST, and static checker, with no
// engine or store dependencies. Execution lives in internal/engine
// (Supervisor.RunMission), which walks the AST and journals every effect
// to the normal session/trajectory store.
//
// The v0 subset covers the executable core of the paper: "agent fn"
// declarations (worker / costs / prompt), "type" record declarations
// (enforced as JSON Schema at the agent boundary), and missions with
// policy, budget, let (with "retry n on E"), emit, "gate human",
// "with cap ... from policy" blocks, "until dry(n)" discovery loops, and
// "on" error handlers. In expression position, calls compose with
// "par for" fan-out and "judge ... by ... require k of n" verification.
// The hello-world slice:
//
//	agent fn greet(name: Text) -> Text
//	  worker any(claude, gemini)
//	  costs <= 4k tokens
//	  prompt """
//	  Say "hello, ${name}" back.
//	  """
//
//	mission hello_world {
//	  budget 20k tokens, $1, 5min
//	  let greeting = greet("world")
//	  emit greeting
//	}
//
// The RFC constructs still outside the subset (host "fn", "context",
// "recall", "var", "if") parse to a clear "not yet implemented"
// diagnostic rather than a generic syntax error, so programs written
// against the paper degrade legibly.
package steer

import (
	"fmt"
	"strings"
	"time"
)

// Pos is a source position, 1-based.
type Pos struct {
	Line int
	Col  int
}

// Diag is one diagnostic (parse or check error/warning) with enough
// context to print a compiler-style message with a source excerpt.
type Diag struct {
	File    string
	Pos     Pos
	Msg     string
	SrcLine string // the offending source line, for the caret excerpt
	Warning bool
}

func (d Diag) Error() string {
	sev := "error"
	if d.Warning {
		sev = "warning"
	}
	return fmt.Sprintf("%s:%d:%d: %s: %s", d.File, d.Pos.Line, d.Pos.Col, sev, d.Msg)
}

// Render formats the diagnostic with a source excerpt and caret:
//
//	hello.steer:9:7: error: unknown function "gret" (did you mean "greet"?)
//	  let greeting = gret("world")
//	                 ^
func (d Diag) Render() string {
	var b strings.Builder
	b.WriteString(d.Error())
	if d.SrcLine != "" {
		b.WriteString("\n  ")
		b.WriteString(d.SrcLine)
		b.WriteString("\n  ")
		for i := 0; i < d.Pos.Col-1 && i < len(d.SrcLine); i++ {
			if d.SrcLine[i] == '\t' {
				b.WriteByte('\t')
			} else {
				b.WriteByte(' ')
			}
		}
		b.WriteString("^")
	}
	return b.String()
}

// Program is one parsed .steer file: type + agent fn declarations plus
// exactly one mission (the checker enforces the "exactly one").
type Program struct {
	File    string
	Types   []*RecordType
	Agents  []*AgentFn
	Mission *Mission
}

// RecordType is a declared record: it doubles as the JSON Schema enforced
// at the agent boundary when an agent fn returns it.
//
//	type Finding { title: Text, file: Text, severity: Text }
type RecordType struct {
	Pos    Pos
	Name   string
	Fields []RecordField
}

// RecordField types are Text, Int, or Bool in v0.
type RecordField struct {
	Pos  Pos
	Name string
	Type string
}

// AgentFn is declaration form 2 from the paper: the body is a goal,
// execution is inference. Workers is the provider set from the worker
// clause (empty = "whatever the runtime defaults to"); CostTokens is the
// per-call ceiling from `costs <= N tokens` (0 = uncapped).
type AgentFn struct {
	Pos        Pos
	Name       string
	Params     []Param
	ReturnType string
	Workers    []string
	CostTokens int64
	Prompt     PromptBlock
}

// Param is one declared parameter. Types are contracts-in-waiting: v0
// records them and checks arity, not shapes — except the @k refinement,
// which the checker enforces at call sites.
type Param struct {
	Pos  Pos
	Name string
	Type string
	// Refine is the @k refinement suffix (`fixes: [Fix@2]`): the argument
	// must be a value that passed `judge ... require K of N` with K >= Refine.
	// The subtype chain is @2 <: @1 <: @0 — more judgment is always
	// acceptable, less never is. 0 (the default) accepts anything.
	Refine int
	// Lenses is the named form of the demand (`fixes: [Fix @ {Security,
	// Repro}]`) — the provenance lattice that extends the @k count with
	// WHICH lenses must have let the value stand. When set, Refine ==
	// len(Lenses): the count is the lattice's necessary condition, and the
	// checker additionally requires the argument's statically guaranteed
	// lens set to cover every name here (guaranteed only by a
	// `require N of N` judge — see lensSetOf). Declaration order is
	// preserved.
	Lenses []string
}

// TypeString renders the declared type with its refinement suffix as
// written in the source: "[Fix@2]", "Fix@{Security, Repro}", or plain
// "Text".
func (p Param) TypeString() string {
	var suffix string
	switch {
	case len(p.Lenses) > 0:
		suffix = "@{" + strings.Join(p.Lenses, ", ") + "}"
	case p.Refine > 0:
		suffix = fmt.Sprintf("@%d", p.Refine)
	default:
		return p.Type
	}
	if strings.HasPrefix(p.Type, "[") && strings.HasSuffix(p.Type, "]") {
		return p.Type[:len(p.Type)-1] + suffix + "]"
	}
	return p.Type + suffix
}

// PromptBlock is the triple-quoted prompt with its ${name} interpolation
// slots pre-extracted (so the checker can verify them and the runtime can
// render without re-scanning).
type PromptBlock struct {
	Pos   Pos
	Text  string
	Slots []Slot
}

// Slot is one ${name} occurrence inside a prompt block.
type Slot struct {
	Pos  Pos // position of the ${ in the source
	Name string
}

// Mission is the top-level unit: journaled, resumable, addressable —
// a session with source code.
type Mission struct {
	Pos      Pos
	Name     string
	Policy   *PolicyDecl
	Budget   *BudgetDecl
	Stmts    []Stmt
	Handlers []*OnHandler
}

// OnHandler is a mission-level compensation block:
//
//	on BudgetExceeded -> emit partial; halt
//
// When the mission walk fails with the named typed error, the handler's
// statements run before the session closes — already-bound lets are still
// visible, so a partial result can be emitted. A `halt` in the body turns
// the failure into a clean typed termination; without it the original
// error still fails the mission after the compensation runs.
type OnHandler struct {
	Pos     Pos
	ErrName string // typed error caught (BudgetExceeded, GateDenied, …)
	ErrPos  Pos    // position of the error name, for diagnostics
	Body    []Stmt // let / emit / halt, separated by ';'
}

// PolicyDecl binds the mission to a named MissionProfile — the static
// scope carrying the mission's capability grants:
//
//	policy dev
//
// The name resolves to a profile under ~/.uta/profiles/ or
// <project>/.uta/profiles/. Resolution is not this package's job (profile
// discovery reads the filesystem; the checker stays pure) — callers load
// the installed profiles and hand their names to CheckPolicy. At run time
// the profile's allowed/denied tool lists feed the engine's capability
// gate, which inspects every tool invocation the mission's workers make.
type PolicyDecl struct {
	Pos  Pos
	Name string
}

// BudgetDecl is the mission's linear resource envelope. Zero fields mean
// "that dimension uncapped" — but the checker requires at least one of
// Tokens/USDCents to be set (budget is the memory-safety of the agent era).
type BudgetDecl struct {
	Pos      Pos
	Tokens   int64
	USDCents int64
	Deadline time.Duration
}

// Stmt is one mission-body statement.
type Stmt interface{ stmtPos() Pos }

// LetStmt binds the value of an expression to a name.
type LetStmt struct {
	Pos  Pos
	Name string
	Expr Expr
}

// EmitStmt produces the mission's public result.
type EmitStmt struct {
	Pos  Pos
	Expr Expr
}

// GateStmt suspends the mission until a human signs off:
//
//	gate human "Publish this draft?"
//	gate human "Publish this draft?" within 24h
//	gate choose "Ship or hold?" of ["ship", "hold"]
//
// The runtime writes a pending HITL request and stops the walk in a
// re-enterable state; `uta hitl approve <session>` plus
// `--resume-session` carries the mission past the gate, a denial fails
// it with a typed error. Within, when non-zero, is the answer deadline:
// resuming an unanswered gate past it fails with the typed GateTimedOut
// error instead of re-suspending, so `on GateTimedOut -> …` compensates.
//
// Options non-nil makes the gate a `gate choose`: the human answers by
// naming one of the enumerated options (`uta hitl approve --option X`
// writes it as the approval marker's content) instead of a bare y/N,
// and the journal records the choice. The checker rejects empty and
// duplicate option lists; nil means a plain `gate human`.
type GateStmt struct {
	Pos     Pos
	Message Expr
	Within  time.Duration // 0 = no deadline
	Options []string      // non-nil = gate choose
}

// WithStmt is a scoped capability raise:
//
//	with cap "Bash(git push *)" from policy release {
//	  let out = deploy(plan)
//	}
//
// Inside the block the named capability joins the allow set of the
// mission's active capability gate; on exit the raise is dropped. The
// checker (CheckCapRaises) verifies the named policy actually grants the
// capability, so an unbacked raise fails at `uta mission check`, not at
// run time. The runtime journals a cap_raised / cap_dropped event pair
// around the block, so the trajectory shows exactly which calls ran with
// elevated authority.
type WithStmt struct {
	Pos    Pos
	Cap    string // tool pattern being raised (capability gate grammar)
	Policy string // the MissionProfile the raise borrows the grant from
	Body   []Stmt
}

// UntilStmt is the discovery loop primitive:
//
//	until dry(2) {
//	  let more = find_bugs("security")
//	}
//
// The body runs in rounds until N consecutive rounds come up dry — a
// round is dry when it yields nothing or repeats the previous round
// verbatim — or until the mission budget is exhausted, whichever comes
// first. Budget exhaustion terminates the loop, not the mission: the
// results accumulated so far stay bound, so a following emit publishes
// the partial yield. The checker refuses the construct in a mission
// without a token or dollar ceiling — an unguarded discovery loop has
// no termination bound.
type UntilStmt struct {
	Pos  Pos
	N    int // consecutive dry rounds that end the loop
	Body []Stmt
}

// HaltStmt ends the mission cleanly from inside an on-handler — a typed
// termination distinct from failure. The parser only accepts it there.
type HaltStmt struct {
	Pos Pos
}

func (s *LetStmt) stmtPos() Pos   { return s.Pos }
func (s *EmitStmt) stmtPos() Pos  { return s.Pos }
func (s *GateStmt) stmtPos() Pos  { return s.Pos }
func (s *WithStmt) stmtPos() Pos  { return s.Pos }
func (s *UntilStmt) stmtPos() Pos { return s.Pos }
func (s *HaltStmt) stmtPos() Pos  { return s.Pos }

// Expr is a v0 expression: string literal, identifier, or agent call.
type Expr interface{ exprPos() Pos }

// StringLit is a quoted string value.
type StringLit struct {
	Pos   Pos
	Value string
}

// Ident references a let-bound name (or, inside prompts, a parameter).
type Ident struct {
	Pos  Pos
	Name string
}

// CallExpr invokes a declared agent fn.
type CallExpr struct {
	Pos  Pos
	Name string
	Args []Expr
}

// ParForExpr is structured fan-out: the body expression runs once per
// item, concurrently, with Var bound to the item. The expression's value
// is the branch results joined in item order. List literals are only
// legal as the iteration source (the v0 value model stays Text).
//
//	let raw = par for dim in ["security", "perf"] { find_bugs(dim) }
type ParForExpr struct {
	Pos   Pos
	Var   string
	Items []Expr
	Body  Expr
}

// JudgeExpr is verification as an expression: N verifier calls run
// concurrently against a value; the value passes through when at least K
// let it stand, otherwise the judge fails with a typed rejection.
//
//	let real = judge finding by refute("security"), refute("repro") require 2 of 2
//
// Each By call names an agent fn whose LAST declared parameter receives
// the judged value — so a 2-param fn is written with 1 argument here. The
// runtime appends the verdict contract (STANDS / REFUTED, default to
// REFUTED when uncertain) to each verifier's prompt.
type JudgeExpr struct {
	Pos   Pos
	Value Expr
	By    []*CallExpr
	K, N  int
}

// RetryExpr is typed failure handling: bounded re-evaluation of the
// wrapped expression when it fails with the named typed error.
//
//	let fixes = fix(plan)
//	            retry 2 on SchemaMismatch
//
// The expression runs up to N+1 times; only a failure matching ErrName
// triggers another attempt, anything else propagates immediately, and
// exhaustion propagates the original typed error. The checker rejects
// unknown error names and BudgetExceeded (the budget is already spent —
// a retry could only overspend further).
type RetryExpr struct {
	Pos     Pos
	N       int    // additional attempts after the first failure
	ErrName string // typed error caught (SchemaMismatch, JudgeRejected)
	ErrPos  Pos    // position of the error name, for diagnostics
	Expr    Expr
}

// LensName reports the provenance lens a judge verifier call represents:
// the first argument when it is a string literal (the `refute("security")`
// shape), the verifier's fn name otherwise. The runtime tags judge-passed
// values with these names in the journal; the static lens-set checker
// (lensSetOf) predicts the same names — one definition here is what keeps
// the runtime truth and the prediction from drifting.
func LensName(c *CallExpr) string {
	if len(c.Args) > 0 {
		if s, ok := c.Args[0].(*StringLit); ok {
			return s.Value
		}
	}
	return c.Name
}

func (e *StringLit) exprPos() Pos  { return e.Pos }
func (e *Ident) exprPos() Pos      { return e.Pos }
func (e *CallExpr) exprPos() Pos   { return e.Pos }
func (e *ParForExpr) exprPos() Pos { return e.Pos }
func (e *JudgeExpr) exprPos() Pos  { return e.Pos }
func (e *RetryExpr) exprPos() Pos  { return e.Pos }

// RenderPrompt substitutes the prompt's ${name} slots from the given
// bindings. The checker guarantees every slot resolves, so a missing
// binding here is a programming error in the runtime, reported verbatim.
// Substitution is a single pass over the template: a binding value that
// itself contains ${other} stays literal rather than expanding into
// another slot's value.
func (a *AgentFn) RenderPrompt(bindings map[string]string) (string, error) {
	pairs := make([]string, 0, 2*len(a.Prompt.Slots))
	seen := make(map[string]bool, len(a.Prompt.Slots))
	for _, s := range a.Prompt.Slots {
		if seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		v, ok := bindings[s.Name]
		if !ok {
			return "", fmt.Errorf("prompt slot ${%s} has no binding (checker should have caught this)", s.Name)
		}
		pairs = append(pairs, "${"+s.Name+"}", v)
	}
	return strings.NewReplacer(pairs...).Replace(a.Prompt.Text), nil
}

// Agent returns the declared agent fn by name, or nil.
func (p *Program) Agent(name string) *AgentFn {
	for _, a := range p.Agents {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// Type returns the declared record type by name, or nil.
func (p *Program) Type(name string) *RecordType {
	for _, t := range p.Types {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// SchemaFor resolves an agent fn's return type to its record schema.
// structured=false means the fn returns plain Text (or a text list) and
// no boundary validation applies.
func (p *Program) SchemaFor(fn *AgentFn) (rec *RecordType, isList, structured bool) {
	name := fn.ReturnType
	if strings.HasPrefix(name, "[") && strings.HasSuffix(name, "]") {
		name = name[1 : len(name)-1]
		isList = true
	}
	if t := p.Type(name); t != nil {
		return t, isList, true
	}
	return nil, false, false
}

// Calls returns the mission's agent calls in execution order — nested
// arguments run before the call that consumes them. This is the static
// plan: the runtime announces it up front and dry-run prints it.
func (m *Mission) Calls() []*CallExpr {
	var out []*CallExpr
	var walk func(e Expr)
	walk = func(e Expr) {
		switch x := e.(type) {
		case *CallExpr:
			for _, a := range x.Args {
				walk(a)
			}
			out = append(out, x)
		case *ParForExpr:
			for _, it := range x.Items {
				walk(it)
			}
			walk(x.Body) // the body call appears once; it runs per item
		case *JudgeExpr:
			walk(x.Value)
			for _, by := range x.By {
				walk(by)
			}
		case *RetryExpr:
			walk(x.Expr) // the call appears once; attempts re-run it
		}
	}
	var walkStmts func(stmts []Stmt)
	walkStmts = func(stmts []Stmt) {
		for _, st := range stmts {
			switch s := st.(type) {
			case *LetStmt:
				walk(s.Expr)
			case *EmitStmt:
				walk(s.Expr)
			case *GateStmt:
				walk(s.Message)
			case *WithStmt:
				walkStmts(s.Body)
			case *UntilStmt:
				walkStmts(s.Body) // each body call appears once; it runs per round
			}
		}
	}
	walkStmts(m.Stmts)
	for _, h := range m.Handlers {
		walkStmts(h.Body) // compensation calls run only on that error, but belong in the plan
	}
	return out
}

// CapRaises returns every `with cap` block in the mission, outermost
// first, nested blocks included.
func (m *Mission) CapRaises() []*WithStmt {
	var out []*WithStmt
	var walkStmts func(stmts []Stmt)
	walkStmts = func(stmts []Stmt) {
		for _, st := range stmts {
			switch s := st.(type) {
			case *WithStmt:
				out = append(out, s)
				walkStmts(s.Body)
			case *UntilStmt:
				walkStmts(s.Body)
			}
		}
	}
	walkStmts(m.Stmts)
	return out
}
