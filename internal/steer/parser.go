package steer

import (
	"fmt"
	"strings"
	"time"
)

// reservedConstructs are Paper № 02 constructs the v0 interpreter does not
// execute yet. Naming them explicitly turns "syntax error" into a roadmap
// pointer for programs written against the full RFC.
var reservedConstructs = map[string]string{
	"context": "named context bindings",
	"recall":  "memory queries",
	"var":     "mutable bindings", "if": "conditionals",
}

// Parse turns one .steer source file into a Program. It fails fast on the
// first syntax error (returning it as a single Diag); semantic problems are
// the checker's job and are reported in bulk.
func Parse(file, src string) (*Program, *Diag) {
	p := &parser{lx: newLexer(file, src)}
	if d := p.advance(); d != nil {
		return nil, d
	}
	prog := &Program{File: file}
	for p.tok.kind != tokEOF {
		switch {
		case p.tok.kind == tokIdent && p.tok.text == "agent":
			fn, d := p.parseAgentFn()
			if d != nil {
				return nil, d
			}
			prog.Agents = append(prog.Agents, fn)
		case p.tok.kind == tokIdent && p.tok.text == "mission":
			m, d := p.parseMission()
			if d != nil {
				return nil, d
			}
			if prog.Mission != nil {
				d := p.errHere("multiple missions in one file; v0 allows exactly one")
				return nil, &d
			}
			prog.Mission = m
		case p.tok.kind == tokIdent && p.tok.text == "type":
			t, d := p.parseTypeDecl()
			if d != nil {
				return nil, d
			}
			prog.Types = append(prog.Types, t)
		case p.tok.kind == tokIdent && p.tok.text == "fn":
			d := p.errHere("host fn declarations are not implemented in the v0 interpreter (only agent fn)")
			return nil, &d
		case p.tok.kind == tokIdent && reservedConstructs[p.tok.text] != "":
			d := p.errHere("%q (%s) is in the steer RFC but not implemented in the v0 interpreter", p.tok.text, reservedConstructs[p.tok.text])
			return nil, &d
		default:
			d := p.errHere("expected 'agent fn' or 'mission' at top level, got %s", p.describe())
			return nil, &d
		}
	}
	return prog, nil
}

type parser struct {
	lx  *lexer
	tok token
}

func (p *parser) advance() *Diag {
	t, d := p.lx.next()
	if d != nil {
		return d
	}
	p.tok = t
	return nil
}

func (p *parser) describe() string {
	switch p.tok.kind {
	case tokIdent:
		return fmt.Sprintf("%q", p.tok.text)
	case tokNumber:
		return fmt.Sprintf("number %d", p.tok.num)
	case tokString:
		return fmt.Sprintf("string %q", p.tok.text)
	default:
		return tokNames[p.tok.kind]
	}
}

func (p *parser) errHere(format string, args ...any) Diag {
	return p.lx.errAt(p.tok.pos, format, args...)
}

func (p *parser) expect(k tokKind, ctx string) (token, *Diag) {
	if p.tok.kind != k {
		d := p.errHere("expected %s %s, got %s", tokNames[k], ctx, p.describe())
		return token{}, &d
	}
	t := p.tok
	if d := p.advance(); d != nil {
		return token{}, d
	}
	return t, nil
}

func (p *parser) expectKeyword(word, ctx string) *Diag {
	if p.tok.kind != tokIdent || p.tok.text != word {
		d := p.errHere("expected %q %s, got %s", word, ctx, p.describe())
		return &d
	}
	return p.advance()
}

// parseAgentFn parses:
//
//	agent fn name(p: T, ...) -> T
//	  worker any(claude, gemini)     (optional)
//	  costs <= 80k tokens            (optional)
//	  prompt """…"""                 (required — checked by the checker)
func (p *parser) parseAgentFn() (*AgentFn, *Diag) {
	pos := p.tok.pos
	if d := p.advance(); d != nil { // consume 'agent'
		return nil, d
	}
	if d := p.expectKeyword("fn", "after 'agent'"); d != nil {
		return nil, d
	}
	nameTok, d := p.expect(tokIdent, "as the agent fn name")
	if d != nil {
		return nil, d
	}
	fn := &AgentFn{Pos: pos, Name: nameTok.text}

	if _, d := p.expect(tokLParen, "to open the parameter list"); d != nil {
		return nil, d
	}
	for p.tok.kind != tokRParen {
		pTok, d := p.expect(tokIdent, "as a parameter name")
		if d != nil {
			return nil, d
		}
		if _, d := p.expect(tokColon, "after the parameter name"); d != nil {
			return nil, d
		}
		typ, refine, lenses, d := p.parseType()
		if d != nil {
			return nil, d
		}
		fn.Params = append(fn.Params, Param{Pos: pTok.pos, Name: pTok.text, Type: typ, Refine: refine, Lenses: lenses})
		if p.tok.kind == tokComma {
			if d := p.advance(); d != nil {
				return nil, d
			}
		}
	}
	if d := p.advance(); d != nil { // consume ')'
		return nil, d
	}
	if _, d := p.expect(tokArrow, "before the return type"); d != nil {
		return nil, d
	}
	retPos := p.tok.pos
	ret, retRefine, retLenses, d := p.parseType()
	if d != nil {
		return nil, d
	}
	if retRefine > 0 || len(retLenses) > 0 {
		suffix := fmt.Sprintf("@%d", retRefine)
		if len(retLenses) > 0 {
			suffix = "@{" + strings.Join(retLenses, ", ") + "}"
		}
		dd := p.lx.errAt(retPos, "a return type cannot carry an %s refinement — an agent's own output is always unjudged (@0); refinement is earned at the call site with `judge ... require k of n`", suffix)
		return nil, &dd
	}
	fn.ReturnType = ret

	// Clauses, in any order, each at most once.
	for {
		if p.tok.kind != tokIdent {
			break
		}
		switch p.tok.text {
		case "worker":
			if len(fn.Workers) > 0 {
				dd := p.errHere("duplicate worker clause")
				return nil, &dd
			}
			if d := p.advance(); d != nil {
				return nil, d
			}
			ws, d := p.parseWorkerSpec()
			if d != nil {
				return nil, d
			}
			fn.Workers = ws
		case "costs":
			if fn.CostTokens > 0 {
				dd := p.errHere("duplicate costs clause")
				return nil, &dd
			}
			if d := p.advance(); d != nil {
				return nil, d
			}
			if _, d := p.expect(tokLE, "after 'costs'"); d != nil {
				return nil, d
			}
			n, d := p.expect(tokNumber, "as the token ceiling")
			if d != nil {
				return nil, d
			}
			if d := p.expectKeyword("tokens", "after the costs ceiling"); d != nil {
				return nil, d
			}
			fn.CostTokens = n.num
		case "prompt":
			if fn.Prompt.Text != "" {
				dd := p.errHere("duplicate prompt clause")
				return nil, &dd
			}
			if d := p.advance(); d != nil {
				return nil, d
			}
			blk, d := p.expect(tokPrompt, `("""…""") after 'prompt'`)
			if d != nil {
				return nil, d
			}
			fn.Prompt = PromptBlock{Pos: blk.pos, Text: blk.text, Slots: extractSlots(blk)}
		default:
			return fn, nil // next declaration begins
		}
	}
	return fn, nil
}

// parseType parses a type with an optional refinement suffix on the base
// or element type: Text, [Fix], Fix@2, [Fix@2], [Fix @ {Security, Repro}].
// The returned string is the bare type ("[Fix]") — the refinement travels
// separately so schema resolution never sees it.
func (p *parser) parseType() (string, int, []string, *Diag) {
	if p.tok.kind == tokLBracket {
		if d := p.advance(); d != nil {
			return "", 0, nil, d
		}
		inner, d := p.expect(tokIdent, "as the element type")
		if d != nil {
			return "", 0, nil, d
		}
		refine, lenses, d := p.parseRefinement()
		if d != nil {
			return "", 0, nil, d
		}
		if _, d := p.expect(tokRBracket, "to close the list type"); d != nil {
			return "", 0, nil, d
		}
		return "[" + inner.text + "]", refine, lenses, nil
	}
	t, d := p.expect(tokIdent, "as a type")
	if d != nil {
		return "", 0, nil, d
	}
	refine, lenses, d := p.parseRefinement()
	if d != nil {
		return "", 0, nil, d
	}
	return t.text, refine, lenses, nil
}

// parseRefinement parses an optional refinement suffix after a type name:
// `@N` (the M5 judged-ness count) or `@{Lens, Lens}` (the provenance
// lattice's named-lens set). A lens set implies its count — the returned
// level is len(lenses), the lattice's necessary condition; the checker
// enforces the named half on top of it. Absent means @0 (anything goes).
func (p *parser) parseRefinement() (int, []string, *Diag) {
	if p.tok.kind != tokAt {
		return 0, nil, nil
	}
	if d := p.advance(); d != nil { // consume '@'
		return 0, nil, d
	}
	if p.tok.kind == tokLBrace {
		if d := p.advance(); d != nil { // consume '{'
			return 0, nil, d
		}
		var lenses []string
		for {
			l, d := p.expect(tokIdent, "as a lens name in the @{...} set")
			if d != nil {
				return 0, nil, d
			}
			for _, seen := range lenses {
				if seen == l.text {
					dd := p.lx.errAt(l.pos, "duplicate lens %q in the @{...} set", l.text)
					return 0, nil, &dd
				}
			}
			lenses = append(lenses, l.text)
			if p.tok.kind == tokComma {
				if d := p.advance(); d != nil {
					return 0, nil, d
				}
				continue
			}
			break
		}
		if _, d := p.expect(tokRBrace, "to close the lens set"); d != nil {
			return 0, nil, d
		}
		return len(lenses), lenses, nil
	}
	n, d := p.expect(tokNumber, "as the refinement level after '@'")
	if d != nil {
		return 0, nil, d
	}
	return int(n.num), nil, nil
}

func (p *parser) parseWorkerSpec() ([]string, *Diag) {
	first, d := p.expect(tokIdent, "as a provider name (or any(...))")
	if d != nil {
		return nil, d
	}
	if first.text != "any" {
		return []string{first.text}, nil
	}
	if _, d := p.expect(tokLParen, "after 'any'"); d != nil {
		return nil, d
	}
	var out []string
	for {
		w, d := p.expect(tokIdent, "as a provider name")
		if d != nil {
			return nil, d
		}
		out = append(out, w.text)
		if p.tok.kind == tokComma {
			if d := p.advance(); d != nil {
				return nil, d
			}
			continue
		}
		break
	}
	if _, d := p.expect(tokRParen, "to close the provider set"); d != nil {
		return nil, d
	}
	return out, nil
}

// parseMission parses:
//
//	mission name {
//	  policy dev
//	  budget 20k tokens, $1, 5min
//	  let x = call("…")
//	  emit x
//	}
func (p *parser) parseMission() (*Mission, *Diag) {
	pos := p.tok.pos
	if d := p.advance(); d != nil { // consume 'mission'
		return nil, d
	}
	nameTok, d := p.expect(tokIdent, "as the mission name")
	if d != nil {
		return nil, d
	}
	m := &Mission{Pos: pos, Name: nameTok.text}
	if _, d := p.expect(tokLBrace, "to open the mission body"); d != nil {
		return nil, d
	}
	for p.tok.kind != tokRBrace {
		if p.tok.kind == tokEOF {
			dd := p.errHere("unexpected end of file inside mission %q (missing '}')", m.Name)
			return nil, &dd
		}
		if p.tok.kind != tokIdent {
			dd := p.errHere("expected a statement, got %s", p.describe())
			return nil, &dd
		}
		switch p.tok.text {
		case "policy":
			if m.Policy != nil {
				dd := p.errHere("duplicate policy declaration (a mission binds at most one profile)")
				return nil, &dd
			}
			polPos := p.tok.pos
			if d := p.advance(); d != nil {
				return nil, d
			}
			n, d := p.expect(tokIdent, "as the policy (MissionProfile) name")
			if d != nil {
				return nil, d
			}
			m.Policy = &PolicyDecl{Pos: polPos, Name: n.text}
		case "budget":
			if m.Budget != nil {
				dd := p.errHere("duplicate budget declaration")
				return nil, &dd
			}
			b, d := p.parseBudget()
			if d != nil {
				return nil, d
			}
			m.Budget = b
		case "let", "emit", "gate", "with", "until":
			st, d := p.parseStmt()
			if d != nil {
				return nil, d
			}
			m.Stmts = append(m.Stmts, st)
		case "on":
			h, d := p.parseOn()
			if d != nil {
				return nil, d
			}
			m.Handlers = append(m.Handlers, h)
		default:
			if why := reservedConstructs[p.tok.text]; why != "" {
				dd := p.errHere("%q (%s) is in the steer RFC but not implemented in the v0 interpreter", p.tok.text, why)
				return nil, &dd
			}
			dd := p.errHere("expected 'policy', 'budget', 'let', 'gate', 'with', 'until', 'on', or 'emit', got %s", p.describe())
			return nil, &dd
		}
	}
	if d := p.advance(); d != nil { // consume '}'
		return nil, d
	}
	return m, nil
}

// parseStmt parses one executable statement — let, emit, gate, with, or
// until — shared between the mission body and block bodies (`with cap`,
// `until dry`), which may nest all five but not the policy/budget
// declarations.
func (p *parser) parseStmt() (Stmt, *Diag) {
	switch p.tok.text {
	case "let":
		letPos := p.tok.pos
		if d := p.advance(); d != nil {
			return nil, d
		}
		n, d := p.expect(tokIdent, "as the binding name")
		if d != nil {
			return nil, d
		}
		if _, d := p.expect(tokAssign, "after the binding name"); d != nil {
			return nil, d
		}
		e, d := p.parseExpr()
		if d != nil {
			return nil, d
		}
		e, d = p.parseRetryClause(e)
		if d != nil {
			return nil, d
		}
		return &LetStmt{Pos: letPos, Name: n.text, Expr: e}, nil
	case "emit":
		emitPos := p.tok.pos
		if d := p.advance(); d != nil {
			return nil, d
		}
		e, d := p.parseExpr()
		if d != nil {
			return nil, d
		}
		return &EmitStmt{Pos: emitPos, Expr: e}, nil
	case "gate":
		gatePos := p.tok.pos
		if d := p.advance(); d != nil {
			return nil, d
		}
		choose := p.tok.kind == tokIdent && p.tok.text == "choose"
		if choose {
			if d := p.advance(); d != nil {
				return nil, d
			}
		} else if d := p.expectKeyword("human", "after 'gate' ('human' or 'choose')"); d != nil {
			return nil, d
		}
		e, d := p.parseExpr()
		if d != nil {
			return nil, d
		}
		var options []string
		if choose {
			options, d = p.parseOptionList()
			if d != nil {
				return nil, d
			}
		}
		within, d := p.parseWithinClause()
		if d != nil {
			return nil, d
		}
		return &GateStmt{Pos: gatePos, Message: e, Within: within, Options: options}, nil
	case "with":
		return p.parseWith()
	case "until":
		return p.parseUntil()
	}
	dd := p.errHere("expected a statement, got %s", p.describe())
	return nil, &dd
}

// parseRetryClause wraps an already-parsed let expression when a
// `retry N on E` handler follows it (RFC: handler = "retry" INT "on" IDENT):
//
//	let fixes = fix(plan)
//	            retry 2 on SchemaMismatch
//
// There is no statement terminator, so the clause binds to the nearest
// preceding let expression; `retry` cannot begin a statement, which keeps
// the attachment unambiguous.
func (p *parser) parseRetryClause(e Expr) (Expr, *Diag) {
	if p.tok.kind != tokIdent || p.tok.text != "retry" {
		return e, nil
	}
	pos := p.tok.pos
	if d := p.advance(); d != nil { // consume 'retry'
		return nil, d
	}
	n, d := p.expect(tokNumber, "as the retry count")
	if d != nil {
		return nil, d
	}
	if n.num < 1 {
		dd := p.lx.errAt(n.pos, "retry needs at least one attempt")
		return nil, &dd
	}
	if d := p.expectKeyword("on", "after the retry count"); d != nil {
		return nil, d
	}
	errTok, d := p.expect(tokIdent, "as the error name (e.g. SchemaMismatch)")
	if d != nil {
		return nil, d
	}
	return &RetryExpr{Pos: pos, N: int(n.num), ErrName: errTok.text, ErrPos: errTok.pos, Expr: e}, nil
}

// parseOptionList parses a choose gate's enumerated answers —
// `gate choose "…" of ["ship", "hold"]`. Options are string literals
// only: the list is the human's answer vocabulary, so it must be static.
// The returned slice is never nil (an empty `of []` parses and is
// rejected by the checker, which owns list-shape diagnostics).
func (p *parser) parseOptionList() ([]string, *Diag) {
	if d := p.expectKeyword("of", "after the gate choose message"); d != nil {
		return nil, d
	}
	if _, d := p.expect(tokLBracket, "to open the option list"); d != nil {
		return nil, d
	}
	options := []string{}
	for p.tok.kind != tokRBracket {
		s, d := p.expect(tokString, "as a gate choose option")
		if d != nil {
			return nil, d
		}
		options = append(options, s.text)
		if p.tok.kind == tokComma {
			if d := p.advance(); d != nil {
				return nil, d
			}
		}
	}
	if d := p.advance(); d != nil { // consume ']'
		return nil, d
	}
	return options, nil
}

// parseWithinClause parses a gate's optional answer deadline —
// `gate human "…" within 24h` — using the same number-plus-unit lexing
// as the budget duration. Returns 0 when no `within` follows the gate.
func (p *parser) parseWithinClause() (time.Duration, *Diag) {
	if p.tok.kind != tokIdent || p.tok.text != "within" {
		return 0, nil
	}
	if d := p.advance(); d != nil { // consume 'within'
		return 0, d
	}
	n, d := p.expect(tokNumber, "as the gate deadline (e.g. within 24h)")
	if d != nil {
		return 0, d
	}
	if n.num < 1 {
		dd := p.lx.errAt(n.pos, "gate deadline must be positive")
		return 0, &dd
	}
	unit, d := p.expect(tokIdent, "after the deadline amount (min, s, h)")
	if d != nil {
		return 0, d
	}
	switch unit.text {
	case "min", "m":
		return time.Duration(n.num) * time.Minute, nil
	case "s", "sec":
		return time.Duration(n.num) * time.Second, nil
	case "h":
		return time.Duration(n.num) * time.Hour, nil
	}
	dd := p.lx.errAt(unit.pos, "unknown deadline unit %q (min, s, h)", unit.text)
	return 0, &dd
}

// parseOn parses a mission-level compensation handler:
//
//	on BudgetExceeded -> emit partial_report; halt
//
// The body is a ';'-separated statement list — let, emit, or halt. halt is
// only legal here: a clean termination is a declared reaction to a typed
// failure, not a statement the happy path may reach.
func (p *parser) parseOn() (*OnHandler, *Diag) {
	pos := p.tok.pos
	if d := p.advance(); d != nil { // consume 'on'
		return nil, d
	}
	errTok, d := p.expect(tokIdent, "as the error name (e.g. BudgetExceeded)")
	if d != nil {
		return nil, d
	}
	if _, d := p.expect(tokArrow, "after the error name"); d != nil {
		return nil, d
	}
	h := &OnHandler{Pos: pos, ErrName: errTok.text, ErrPos: errTok.pos}
	for {
		if p.tok.kind != tokIdent {
			dd := p.errHere("expected 'let', 'emit', or 'halt' in an on-handler, got %s", p.describe())
			return nil, &dd
		}
		switch p.tok.text {
		case "halt":
			h.Body = append(h.Body, &HaltStmt{Pos: p.tok.pos})
			if d := p.advance(); d != nil {
				return nil, d
			}
		case "let", "emit":
			st, d := p.parseStmt()
			if d != nil {
				return nil, d
			}
			h.Body = append(h.Body, st)
		default:
			dd := p.errHere("expected 'let', 'emit', or 'halt' in an on-handler, got %s", p.describe())
			return nil, &dd
		}
		if p.tok.kind == tokSemi {
			if d := p.advance(); d != nil {
				return nil, d
			}
			continue
		}
		return h, nil
	}
}

// parseWith parses a scoped capability raise:
//
//	with cap "Bash(git push *)" from policy release {
//	  let out = deploy(plan)
//	}
//
// The capability is a tool pattern (string) or a bare tool name
// (identifier); the policy names the MissionProfile the raise borrows
// the grant from.
func (p *parser) parseWith() (Stmt, *Diag) {
	pos := p.tok.pos
	if d := p.advance(); d != nil { // consume 'with'
		return nil, d
	}
	if d := p.expectKeyword("cap", "after 'with'"); d != nil {
		return nil, d
	}
	var capName string
	switch p.tok.kind {
	case tokString, tokIdent:
		capName = p.tok.text
		if d := p.advance(); d != nil {
			return nil, d
		}
	default:
		dd := p.errHere("expected a capability (tool pattern string or tool name) after 'with cap', got %s", p.describe())
		return nil, &dd
	}
	if d := p.expectKeyword("from", "after the capability"); d != nil {
		return nil, d
	}
	if d := p.expectKeyword("policy", "after 'from'"); d != nil {
		return nil, d
	}
	polTok, d := p.expect(tokIdent, "as the granting policy (MissionProfile) name")
	if d != nil {
		return nil, d
	}
	ws := &WithStmt{Pos: pos, Cap: capName, Policy: polTok.text}
	if _, d := p.expect(tokLBrace, "to open the with block"); d != nil {
		return nil, d
	}
	for p.tok.kind != tokRBrace {
		if p.tok.kind == tokEOF {
			dd := p.errHere("unexpected end of file inside a with block (missing '}')")
			return nil, &dd
		}
		if p.tok.kind != tokIdent {
			dd := p.errHere("expected a statement, got %s", p.describe())
			return nil, &dd
		}
		switch p.tok.text {
		case "let", "emit", "gate", "with", "until":
			st, d := p.parseStmt()
			if d != nil {
				return nil, d
			}
			ws.Body = append(ws.Body, st)
		default:
			if why := reservedConstructs[p.tok.text]; why != "" {
				dd := p.errHere("%q (%s) is in the steer RFC but not implemented in the v0 interpreter", p.tok.text, why)
				return nil, &dd
			}
			dd := p.errHere("expected 'let', 'gate', 'with', 'until', or 'emit' inside a with block, got %s", p.describe())
			return nil, &dd
		}
	}
	if d := p.advance(); d != nil { // consume '}'
		return nil, d
	}
	if len(ws.Body) == 0 {
		dd := p.lx.errAt(pos, "with block is empty — a raise that scopes no calls grants authority to nothing")
		return nil, &dd
	}
	return ws, nil
}

// parseUntil parses a discovery loop:
//
//	until dry(2) {
//	  let more = find_bugs("security")
//	}
//
// The dry count is the number of consecutive empty rounds that ends the
// loop; the checker separately insists the mission carries a token or
// dollar budget (the loop's other termination bound).
func (p *parser) parseUntil() (Stmt, *Diag) {
	pos := p.tok.pos
	if d := p.advance(); d != nil { // consume 'until'
		return nil, d
	}
	if d := p.expectKeyword("dry", "after 'until' (v0 supports dry loops only)"); d != nil {
		return nil, d
	}
	if _, d := p.expect(tokLParen, "after 'dry'"); d != nil {
		return nil, d
	}
	n, d := p.expect(tokNumber, "as the dry-round count")
	if d != nil {
		return nil, d
	}
	if n.num < 1 {
		dd := p.lx.errAt(n.pos, "until dry needs at least one dry round")
		return nil, &dd
	}
	if _, d := p.expect(tokRParen, "to close the dry-round count"); d != nil {
		return nil, d
	}
	us := &UntilStmt{Pos: pos, N: int(n.num)}
	if _, d := p.expect(tokLBrace, "to open the until body"); d != nil {
		return nil, d
	}
	for p.tok.kind != tokRBrace {
		if p.tok.kind == tokEOF {
			dd := p.errHere("unexpected end of file inside an until body (missing '}')")
			return nil, &dd
		}
		if p.tok.kind != tokIdent {
			dd := p.errHere("expected a statement, got %s", p.describe())
			return nil, &dd
		}
		switch p.tok.text {
		case "let", "emit", "gate", "with", "until":
			st, d := p.parseStmt()
			if d != nil {
				return nil, d
			}
			us.Body = append(us.Body, st)
		default:
			if why := reservedConstructs[p.tok.text]; why != "" {
				dd := p.errHere("%q (%s) is in the steer RFC but not implemented in the v0 interpreter", p.tok.text, why)
				return nil, &dd
			}
			dd := p.errHere("expected 'let', 'gate', 'with', 'until', or 'emit' inside an until body, got %s", p.describe())
			return nil, &dd
		}
	}
	if d := p.advance(); d != nil { // consume '}'
		return nil, d
	}
	if len(us.Body) == 0 {
		dd := p.lx.errAt(pos, "until body is empty — a discovery loop with no statements discovers nothing")
		return nil, &dd
	}
	return us, nil
}

// parseBudget parses `budget 20k tokens, $1, 5min` — items in any order,
// each dimension at most once.
func (p *parser) parseBudget() (*BudgetDecl, *Diag) {
	b := &BudgetDecl{Pos: p.tok.pos}
	if d := p.advance(); d != nil { // consume 'budget'
		return nil, d
	}
	for {
		switch {
		case p.tok.kind == tokDollar:
			if b.USDCents > 0 {
				dd := p.errHere("duplicate dollar budget")
				return nil, &dd
			}
			if d := p.advance(); d != nil {
				return nil, d
			}
			n, d := p.expect(tokNumber, "after '$'")
			if d != nil {
				return nil, d
			}
			cents := n.num * 100
			// $1.50 — a decimal cents part must be exactly two digits, so
			// "$1.5" is rejected rather than silently read as $1.05 or $1.50.
			if p.tok.kind == tokDot {
				if d := p.advance(); d != nil {
					return nil, d
				}
				frac, d := p.expect(tokNumber, "as the cents part after '.'")
				if d != nil {
					return nil, d
				}
				if len(frac.text) != 2 {
					dd := p.lx.errAt(frac.pos, "dollar amounts need exactly two cent digits (write $%d.%02d)", n.num, frac.num)
					return nil, &dd
				}
				cents += frac.num
			}
			b.USDCents = cents
		case p.tok.kind == tokNumber:
			n := p.tok
			if d := p.advance(); d != nil {
				return nil, d
			}
			unit, d := p.expect(tokIdent, "after the budget amount (tokens, min, s, h)")
			if d != nil {
				return nil, d
			}
			switch unit.text {
			case "tokens":
				if b.Tokens > 0 {
					dd := p.lx.errAt(n.pos, "duplicate token budget")
					return nil, &dd
				}
				b.Tokens = n.num
			case "min", "m":
				if b.Deadline > 0 {
					dd := p.lx.errAt(n.pos, "duplicate duration budget")
					return nil, &dd
				}
				b.Deadline = time.Duration(n.num) * time.Minute
			case "s", "sec":
				if b.Deadline > 0 {
					dd := p.lx.errAt(n.pos, "duplicate duration budget")
					return nil, &dd
				}
				b.Deadline = time.Duration(n.num) * time.Second
			case "h":
				if b.Deadline > 0 {
					dd := p.lx.errAt(n.pos, "duplicate duration budget")
					return nil, &dd
				}
				b.Deadline = time.Duration(n.num) * time.Hour
			default:
				dd := p.lx.errAt(unit.pos, "unknown budget unit %q (tokens, min, s, h)", unit.text)
				return nil, &dd
			}
		default:
			dd := p.errHere("expected a budget item (e.g. 20k tokens, $1, 5min), got %s", p.describe())
			return nil, &dd
		}
		if p.tok.kind == tokComma {
			if d := p.advance(); d != nil {
				return nil, d
			}
			continue
		}
		return b, nil
	}
}

func (p *parser) parseExpr() (Expr, *Diag) {
	switch p.tok.kind {
	case tokString:
		t := p.tok
		if d := p.advance(); d != nil {
			return nil, d
		}
		return &StringLit{Pos: t.pos, Value: t.text}, nil
	case tokIdent:
		t := p.tok
		if t.text == "par" {
			return p.parseParFor()
		}
		if t.text == "judge" {
			return p.parseJudge()
		}
		if d := p.advance(); d != nil {
			return nil, d
		}
		if p.tok.kind != tokLParen {
			return &Ident{Pos: t.pos, Name: t.text}, nil
		}
		if d := p.advance(); d != nil { // consume '('
			return nil, d
		}
		call := &CallExpr{Pos: t.pos, Name: t.text}
		for p.tok.kind != tokRParen {
			arg, d := p.parseExpr()
			if d != nil {
				return nil, d
			}
			call.Args = append(call.Args, arg)
			if p.tok.kind == tokComma {
				if d := p.advance(); d != nil {
					return nil, d
				}
			}
		}
		if d := p.advance(); d != nil { // consume ')'
			return nil, d
		}
		return call, nil
	default:
		d := p.errHere("expected an expression (string, name, or call), got %s", p.describe())
		return nil, &d
	}
}

// parseTypeDecl parses `type Name { field: Text, count: Int, ok: Bool }`.
// Commas between fields are optional (newlines read naturally).
func (p *parser) parseTypeDecl() (*RecordType, *Diag) {
	pos := p.tok.pos
	if d := p.advance(); d != nil { // consume 'type'
		return nil, d
	}
	nameTok, d := p.expect(tokIdent, "as the type name")
	if d != nil {
		return nil, d
	}
	t := &RecordType{Pos: pos, Name: nameTok.text}
	if _, d := p.expect(tokLBrace, "to open the field list"); d != nil {
		return nil, d
	}
	for p.tok.kind != tokRBrace {
		fTok, d := p.expect(tokIdent, "as a field name")
		if d != nil {
			return nil, d
		}
		if _, d := p.expect(tokColon, "after the field name"); d != nil {
			return nil, d
		}
		fType, d := p.expect(tokIdent, "as the field type (Text, Int, Bool)")
		if d != nil {
			return nil, d
		}
		t.Fields = append(t.Fields, RecordField{Pos: fTok.pos, Name: fTok.text, Type: fType.text})
		if p.tok.kind == tokComma {
			if d := p.advance(); d != nil {
				return nil, d
			}
		}
	}
	if d := p.advance(); d != nil { // consume '}'
		return nil, d
	}
	if len(t.Fields) == 0 {
		dd := p.lx.errAt(pos, "type %q has no fields", t.Name)
		return nil, &dd
	}
	return t, nil
}

// parseParFor parses `par for x in [item, ...] { body }`. The item list is
// syntactic — list values do not exist elsewhere in the v0 value model.
func (p *parser) parseParFor() (Expr, *Diag) {
	pos := p.tok.pos
	if d := p.advance(); d != nil { // consume 'par'
		return nil, d
	}
	if d := p.expectKeyword("for", "after 'par'"); d != nil {
		return nil, d
	}
	v, d := p.expect(tokIdent, "as the loop variable")
	if d != nil {
		return nil, d
	}
	if d := p.expectKeyword("in", "after the loop variable"); d != nil {
		return nil, d
	}
	if _, d := p.expect(tokLBracket, "to open the item list"); d != nil {
		return nil, d
	}
	pf := &ParForExpr{Pos: pos, Var: v.text}
	for p.tok.kind != tokRBracket {
		item, d := p.parseExpr()
		if d != nil {
			return nil, d
		}
		pf.Items = append(pf.Items, item)
		if p.tok.kind == tokComma {
			if d := p.advance(); d != nil {
				return nil, d
			}
		}
	}
	if d := p.advance(); d != nil { // consume ']'
		return nil, d
	}
	if len(pf.Items) == 0 {
		dd := p.lx.errAt(pos, "par for needs at least one item")
		return nil, &dd
	}
	if _, d := p.expect(tokLBrace, "to open the par body"); d != nil {
		return nil, d
	}
	body, d := p.parseExpr()
	if d != nil {
		return nil, d
	}
	pf.Body = body
	if _, d := p.expect(tokRBrace, "to close the par body"); d != nil {
		return nil, d
	}
	return pf, nil
}

// parseJudge parses:
//
//	judge <expr> by call(...), call(...) require K of N
func (p *parser) parseJudge() (Expr, *Diag) {
	pos := p.tok.pos
	if d := p.advance(); d != nil { // consume 'judge'
		return nil, d
	}
	val, d := p.parseExpr()
	if d != nil {
		return nil, d
	}
	if d := p.expectKeyword("by", "after the judged expression"); d != nil {
		return nil, d
	}
	j := &JudgeExpr{Pos: pos, Value: val}
	for {
		e, d := p.parseExpr()
		if d != nil {
			return nil, d
		}
		call, ok := e.(*CallExpr)
		if !ok {
			dd := p.lx.errAt(e.exprPos(), "judge verifiers must be agent fn calls")
			return nil, &dd
		}
		j.By = append(j.By, call)
		if p.tok.kind == tokComma {
			if d := p.advance(); d != nil {
				return nil, d
			}
			continue
		}
		break
	}
	if d := p.expectKeyword("require", "after the verifier list"); d != nil {
		return nil, d
	}
	k, d := p.expect(tokNumber, "as the required stand count")
	if d != nil {
		return nil, d
	}
	if d := p.expectKeyword("of", "after the required count"); d != nil {
		return nil, d
	}
	n, d := p.expect(tokNumber, "as the verifier count")
	if d != nil {
		return nil, d
	}
	j.K, j.N = int(k.num), int(n.num)
	return j, nil
}

// extractSlots finds every ${name} in a prompt block. Slot positions are
// approximated line-accurately within the dedented text (content starts on
// the line after the opening triple quote).
func extractSlots(blk token) []Slot {
	var slots []Slot
	text := blk.text
	line := blk.pos.Line + 1
	for i := 0; i+1 < len(text); i++ {
		if text[i] == '\n' {
			line++
			continue
		}
		if text[i] == '$' && text[i+1] == '{' {
			j := strings.IndexByte(text[i+2:], '}')
			if j < 0 {
				continue
			}
			name := text[i+2 : i+2+j]
			slots = append(slots, Slot{Pos: Pos{Line: line, Col: 1}, Name: name})
			i += 2 + j
		}
	}
	return slots
}
