package steer

import (
	"strings"
	"testing"
)

func findingType(t *testing.T) *RecordType {
	t.Helper()
	src := `type Finding { title: Text, count: Int, blocking: Bool }
agent fn f() -> Finding
  prompt """go"""
mission m {
  budget 1k tokens
  emit f()
}`
	prog := parseOK(t, src)
	if diags := Check(prog, src); HasErrors(diags) {
		t.Fatalf("check: %s", renderAll(diags))
	}
	return prog.Types[0]
}

func TestParse_TypeDecl(t *testing.T) {
	rt := findingType(t)
	if rt.Name != "Finding" || len(rt.Fields) != 3 {
		t.Fatalf("type = %+v", rt)
	}
	if rt.Fields[1].Name != "count" || rt.Fields[1].Type != "Int" {
		t.Errorf("field[1] = %+v", rt.Fields[1])
	}
}

func TestCheck_UnknownReturnTypeErrors(t *testing.T) {
	src := `agent fn f() -> Findng
  prompt """go"""
mission m {
  budget 1k tokens
  emit f()
}`
	prog := parseOK(t, src)
	joined := renderAll(Check(prog, src))
	if !strings.Contains(joined, `returns "Findng"`) {
		t.Errorf("diags = %s", joined)
	}
}

func TestCheck_BadFieldTypeErrors(t *testing.T) {
	src := `type T { x: Float }
agent fn f() -> T
  prompt """go"""
mission m {
  budget 1k tokens
  emit f()
}`
	prog := parseOK(t, src)
	joined := renderAll(Check(prog, src))
	if !strings.Contains(joined, "Text, Int, or Bool") {
		t.Errorf("diags = %s", joined)
	}
}

func TestValidateRecord_Object(t *testing.T) {
	rt := findingType(t)
	out, err := ValidateRecord(rt, `{"title":"x","count":3,"blocking":false,"extra":"ok"}`, false)
	if err != nil {
		t.Fatalf("valid object rejected: %v", err)
	}
	if !strings.Contains(out, `"count":3`) {
		t.Errorf("normalized = %q", out)
	}
}

func TestValidateRecord_StripsCodeFences(t *testing.T) {
	rt := findingType(t)
	raw := "```json\n{\"title\":\"x\",\"count\":1,\"blocking\":true}\n```"
	if _, err := ValidateRecord(rt, raw, false); err != nil {
		t.Fatalf("fenced JSON rejected: %v", err)
	}
}

func TestValidateRecord_Failures(t *testing.T) {
	rt := findingType(t)
	cases := map[string]string{
		`not json at all`:                           "not valid JSON",
		`{"title":"x","count":3}`:                   `missing field "blocking"`,
		`{"title":"x","count":3.5,"blocking":true}`: `"count" must be an integer`,
		`{"title":42,"count":3,"blocking":true}`:    `"title" must be a string`,
		`{"title":"x","count":3,"blocking":"yes"}`:  `"blocking" must be a boolean`,
		`[{"title":"x","count":3,"blocking":true}]`: "expected a JSON object",
		`{"title":"x","count":"3","blocking":true}`: `"count" must be an integer`,
	}
	for raw, want := range cases {
		if _, err := ValidateRecord(rt, raw, false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("raw %q: err = %v, want %q", raw, err, want)
		}
	}
}

func TestValidateRecord_List(t *testing.T) {
	rt := findingType(t)
	good := `[{"title":"a","count":1,"blocking":true},{"title":"b","count":2,"blocking":false}]`
	if _, err := ValidateRecord(rt, good, true); err != nil {
		t.Fatalf("valid list rejected: %v", err)
	}
	bad := `[{"title":"a","count":1,"blocking":true},{"title":"b"}]`
	_, err := ValidateRecord(rt, bad, true)
	if err == nil || !strings.Contains(err.Error(), "element 2") {
		t.Errorf("err = %v, want element-2 failure", err)
	}
	_, err = ValidateRecord(rt, `{"title":"a","count":1,"blocking":true}`, true)
	if err == nil || !strings.Contains(err.Error(), "expected a JSON array") {
		t.Errorf("err = %v", err)
	}
}

func TestSchemaInstruction(t *testing.T) {
	rt := findingType(t)
	single := SchemaInstruction(rt, false)
	if !strings.Contains(single, `"title": string`) || !strings.Contains(single, `"count": integer`) || !strings.Contains(single, `"blocking": boolean`) {
		t.Errorf("instruction = %q", single)
	}
	list := SchemaInstruction(rt, true)
	if !strings.Contains(list, "JSON array") {
		t.Errorf("list instruction = %q", list)
	}
}
