package steer

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFormat_Golden pins the canonical shape over the full v0 construct
// set: types, agent fn clauses, budget normalization, retry, par for,
// judge, gate, with cap, until dry, and on-handlers.
func TestFormat_Golden(t *testing.T) {
	src := `type Finding { title: Text,   blocking: Bool }
agent fn scan(lens: Text) -> [Finding]
  costs <= 40000 tokens
  worker any(claude, gemini)
  prompt """
  Scan through the ${lens} lens.
  """
agent fn refute(claim: Text) -> Text
  worker claude
  prompt """${claim}"""
mission sweep {
  budget $1.50, 300000 tokens, 600s
  let raw = par for lens in ["security", "perf"] { scan(lens) }
  let ok = judge raw by refute(), refute() require 2 of 2 retry 1 on JudgeRejected
  gate human "publish?" within 1440min
  with cap "Bash(git push *)" from policy release { until dry(2) { let more = scan("tail") } }
  emit ok
  on BudgetExceeded -> emit raw; halt
}
`
	want := `type Finding {
  title: Text
  blocking: Bool
}

agent fn scan(lens: Text) -> [Finding]
  worker any(claude, gemini)
  costs <= 40k tokens
  prompt """
  Scan through the ${lens} lens.
  """

agent fn refute(claim: Text) -> Text
  worker claude
  prompt """
  ${claim}
  """

mission sweep {
  budget 300k tokens, $1.50, 10min

  let raw = par for lens in ["security", "perf"] {
    scan(lens)
  }
  let ok = judge raw
    by refute(), refute()
    require 2 of 2 retry 1 on JudgeRejected
  gate human "publish?" within 24h
  with cap "Bash(git push *)" from policy release {
    until dry(2) {
      let more = scan("tail")
    }
  }
  emit ok

  on BudgetExceeded -> emit raw; halt
}
`
	prog, d := Parse("golden.steer", src)
	if d != nil {
		t.Fatal(d.Render())
	}
	got := Format(prog)
	if got != want {
		t.Errorf("Format mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestFormat_RoundTripExamples guards the golden corpus: every shipped
// example formats to a program that parses and checks clean, and a
// second format pass is byte-identical (idempotence).
func TestFormat_RoundTripExamples(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.steer"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no examples/*.steer found — did the examples directory move?")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			prog, d := Parse(filepath.Base(path), string(src))
			if d != nil {
				t.Fatalf("parse: %s", d.Render())
			}
			once := Format(prog)
			prog2, d := Parse(filepath.Base(path), once)
			if d != nil {
				t.Fatalf("formatted output does not parse: %s\n%s", d.Render(), once)
			}
			if diags := Check(prog2, once); HasErrors(diags) {
				t.Fatalf("formatted output does not check: %s\n%s", renderAll(diags), once)
			}
			twice := Format(prog2)
			if once != twice {
				t.Errorf("Format is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
			}
			// The example's semantics must survive: same fns, same call plan.
			if len(prog2.Agents) != len(prog.Agents) || len(prog2.Mission.Calls()) != len(prog.Mission.Calls()) {
				t.Errorf("round trip changed the program: %d/%d agents, %d/%d calls",
					len(prog2.Agents), len(prog.Agents), len(prog2.Mission.Calls()), len(prog.Mission.Calls()))
			}
		})
	}
}

func TestFormat_StringEscapes(t *testing.T) {
	src := `agent fn f(x: Text) -> Text
  prompt """${x}"""
mission m {
  budget 10k tokens
  emit f("a \"b\"\n\tc \\ d")
}
`
	prog, d := Parse("esc.steer", src)
	if d != nil {
		t.Fatal(d.Render())
	}
	once := Format(prog)
	prog2, d := Parse("esc.steer", once)
	if d != nil {
		t.Fatalf("formatted output does not parse: %s\n%s", d.Render(), once)
	}
	want := prog.Mission.Stmts[0].(*EmitStmt).Expr.(*CallExpr).Args[0].(*StringLit).Value
	got := prog2.Mission.Stmts[0].(*EmitStmt).Expr.(*CallExpr).Args[0].(*StringLit).Value
	if got != want {
		t.Errorf("string value changed through format: %q != %q", got, want)
	}
}

func TestHasComments(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"comment", "mission m { // hi\n}", true},
		{"none", "mission m {\n}", false},
		{"shebang only", "#!/usr/bin/env -S uta mission run\nmission m {}", false},
		{"shebang then comment", "#!/usr/bin/env -S uta mission run\n// hi\nmission m {}", true},
		{"slashes in string", `emit "https://example.com"`, false},
		{"slashes in prompt block", "prompt \"\"\"\nsee https://example.com\n\"\"\"", false},
		{"comment after string", `let u = "https://x" // why`, true},
	}
	for _, c := range cases {
		if got := HasComments(c.src); got != c.want {
			t.Errorf("%s: HasComments = %v, want %v", c.name, got, c.want)
		}
	}
}
