package textdiff

import (
	"strings"
	"testing"
)

func TestUnified_Identical(t *testing.T) {
	if d := Unified("x.go", "a\nb\n", "a\nb\n"); d != "" {
		t.Fatalf("expected empty diff, got %q", d)
	}
}

func TestUnified_SingleChange(t *testing.T) {
	a := "package main\n\nfunc main() {\n\tprintln(1)\n}\n"
	b := "package main\n\nfunc main() {\n\tprintln(2)\n}\n"
	d := Unified("main.go", a, b)
	for _, want := range []string{"--- a/main.go", "+++ b/main.go", "@@ -1,5 +1,5 @@", "-\tprintln(1)", "+\tprintln(2)"} {
		if !strings.Contains(d, want) {
			t.Fatalf("diff missing %q:\n%s", want, d)
		}
	}
	add, rm := Stat(a, b)
	if add != 1 || rm != 1 {
		t.Fatalf("stat = +%d -%d, want +1 -1", add, rm)
	}
}

func TestUnified_NewFile(t *testing.T) {
	d := Unified("new.txt", "", "hello\nworld\n")
	if !strings.Contains(d, "@@ -0,0 +1,2 @@") || !strings.Contains(d, "+hello") {
		t.Fatalf("unexpected new-file diff:\n%s", d)
	}
}

func TestUnified_TwoHunks(t *testing.T) {
	var a, b []string
	for i := 0; i < 30; i++ {
		a = append(a, "line")
		b = append(b, "line")
	}
	b[2] = "changed-top"
	b[27] = "changed-bottom"
	d := Unified("f", strings.Join(a, "\n"), strings.Join(b, "\n"))
	if strings.Count(d, "@@") != 4 { // two hunks, two @@ markers each
		t.Fatalf("expected two hunks:\n%s", d)
	}
}
