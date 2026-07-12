// Package steer implements the v0 interpreter front-end for the steer
// language — the programming layer sketched in Paper № 02 on top of the
// uta control verbs ("steer is to the uta verbs what C is to assembly").
//
// This package is pure: lexer, parser, AST, and static checker, with no
// engine or store dependencies. Execution lives in internal/engine
// (Supervisor.RunMission), which walks the AST and journals every effect
// to the normal session/trajectory store.
//
// The v0 subset covers the hello-world slice of the language:
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
// Everything else in the paper (par, judge, until dry, gate human, with
// cap) parses to a clear "not yet implemented" diagnostic rather than a
// generic syntax error, so programs written against the RFC degrade
// legibly.
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
// records them and checks arity, not shapes.
type Param struct {
	Pos  Pos
	Name string
	Type string
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
	Pos    Pos
	Name   string
	Budget *BudgetDecl
	Stmts  []Stmt
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

func (s *LetStmt) stmtPos() Pos  { return s.Pos }
func (s *EmitStmt) stmtPos() Pos { return s.Pos }

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

func (e *StringLit) exprPos() Pos  { return e.Pos }
func (e *Ident) exprPos() Pos      { return e.Pos }
func (e *CallExpr) exprPos() Pos   { return e.Pos }
func (e *ParForExpr) exprPos() Pos { return e.Pos }
func (e *JudgeExpr) exprPos() Pos  { return e.Pos }

// RenderPrompt substitutes the prompt's ${name} slots from the given
// bindings. The checker guarantees every slot resolves, so a missing
// binding here is a programming error in the runtime, reported verbatim.
func (a *AgentFn) RenderPrompt(bindings map[string]string) (string, error) {
	out := a.Prompt.Text
	for _, s := range a.Prompt.Slots {
		v, ok := bindings[s.Name]
		if !ok {
			return "", fmt.Errorf("prompt slot ${%s} has no binding (checker should have caught this)", s.Name)
		}
		out = strings.ReplaceAll(out, "${"+s.Name+"}", v)
	}
	return out, nil
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
		}
	}
	for _, st := range m.Stmts {
		switch s := st.(type) {
		case *LetStmt:
			walk(s.Expr)
		case *EmitStmt:
			walk(s.Expr)
		}
	}
	return out
}
