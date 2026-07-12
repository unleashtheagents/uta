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
	"judge": "verification refinement",
	"until": "discovery loops", "gate": "human gates",
	"with": "capability raises", "retry": "typed failure handling",
	"context": "named context bindings", "policy": "policy scopes",
	"recall": "memory queries",
	"var":    "mutable bindings", "if": "conditionals",
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
		typ, d := p.parseType()
		if d != nil {
			return nil, d
		}
		fn.Params = append(fn.Params, Param{Pos: pTok.pos, Name: pTok.text, Type: typ})
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
	ret, d := p.parseType()
	if d != nil {
		return nil, d
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

func (p *parser) parseType() (string, *Diag) {
	if p.tok.kind == tokLBracket {
		if d := p.advance(); d != nil {
			return "", d
		}
		inner, d := p.expect(tokIdent, "as the element type")
		if d != nil {
			return "", d
		}
		if _, d := p.expect(tokRBracket, "to close the list type"); d != nil {
			return "", d
		}
		return "[" + inner.text + "]", nil
	}
	t, d := p.expect(tokIdent, "as a type")
	if d != nil {
		return "", d
	}
	return t.text, nil
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
			m.Stmts = append(m.Stmts, &LetStmt{Pos: letPos, Name: n.text, Expr: e})
		case "emit":
			emitPos := p.tok.pos
			if d := p.advance(); d != nil {
				return nil, d
			}
			e, d := p.parseExpr()
			if d != nil {
				return nil, d
			}
			m.Stmts = append(m.Stmts, &EmitStmt{Pos: emitPos, Expr: e})
		default:
			if why := reservedConstructs[p.tok.text]; why != "" {
				dd := p.errHere("%q (%s) is in the steer RFC but not implemented in the v0 interpreter", p.tok.text, why)
				return nil, &dd
			}
			dd := p.errHere("expected 'budget', 'let', or 'emit', got %s", p.describe())
			return nil, &dd
		}
	}
	if d := p.advance(); d != nil { // consume '}'
		return nil, d
	}
	return m, nil
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
