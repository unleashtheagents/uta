package cli

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// fmtTestSrc is missionTestSrc with non-canonical spacing (extra blank
// lines, uncondensed token count) and no comments, so fmt has work to do.
const fmtTestSrc = `#!/usr/bin/env -S uta mission run


agent fn greet(name: Text) -> Text
  worker any(claude, gemini)
  costs <= 4000 tokens
  prompt """
  Say "hello, ${name}" back.
  """
mission hello_world {
  budget 20k tokens, $1, 5min
  let greeting = greet("world")
  emit greeting
}
`

func runMissionFmt(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newMissionFmtCmd()
	var out, errb strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errb.String(), err
}

func TestMissionFmt_DiffDefault(t *testing.T) {
	path := writeSteer(t, fmtTestSrc)
	stdout, stderr, err := runMissionFmt(t, path)
	if err != nil {
		t.Fatalf("fmt failed: %v\n%s", err, stderr)
	}
	for _, want := range []string{"--- " + path, "+++ " + path + " (formatted)", "@@", "-  costs <= 4000 tokens", "+  costs <= 4k tokens"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("diff missing %q:\n%s", want, stdout)
		}
	}
	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != fmtTestSrc {
		t.Errorf("diff mode must not touch the file (err=%v)", rerr)
	}
}

func TestMissionFmt_WriteIsCanonicalAndIdempotent(t *testing.T) {
	path := writeSteer(t, fmtTestSrc)
	if _, stderr, err := runMissionFmt(t, path, "--write"); err != nil {
		t.Fatalf("fmt --write failed: %v\n%s", err, stderr)
	}
	once, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(once), "#!/usr/bin/env -S uta mission run\n") {
		t.Errorf("shebang not preserved:\n%s", once)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o755 {
		t.Errorf("file mode not preserved: %v", info.Mode().Perm())
	}
	// A formatted file is a fixed point: the second pass diffs nothing.
	stdout, stderr, err := runMissionFmt(t, path)
	if err != nil {
		t.Fatalf("second fmt failed: %v\n%s", err, stderr)
	}
	if stdout != "" {
		t.Errorf("formatted file should produce no diff, got:\n%s", stdout)
	}
	twice, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Error("fmt is not idempotent on disk")
	}
}

func TestMissionFmt_CheckFormattedExitsZero(t *testing.T) {
	path := writeSteer(t, fmtTestSrc)
	if _, stderr, err := runMissionFmt(t, path, "--write"); err != nil {
		t.Fatalf("fmt --write failed: %v\n%s", err, stderr)
	}
	stdout, stderr, err := runMissionFmt(t, path, "--check")
	if err != nil {
		t.Fatalf("--check on a formatted file must exit 0: %v\n%s", err, stderr)
	}
	if stdout != "" {
		t.Errorf("--check on a formatted file should print nothing, got:\n%s", stdout)
	}
}

func TestMissionFmt_CheckUnformattedExitsOne(t *testing.T) {
	path := writeSteer(t, fmtTestSrc)
	stdout, _, err := runMissionFmt(t, path, "--check")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("--check on an unformatted file must exit 1, got: %v", err)
	}
	if !strings.Contains(stdout, path) {
		t.Errorf("--check should name the unformatted file, got:\n%s", stdout)
	}
	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != fmtTestSrc {
		t.Errorf("--check must not touch the file (err=%v)", rerr)
	}
}

func TestMissionFmt_CheckAndWriteRejected(t *testing.T) {
	path := writeSteer(t, fmtTestSrc)
	if _, _, err := runMissionFmt(t, path, "--check", "--write"); err == nil {
		t.Fatal("--check with --write must be rejected")
	}
	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != fmtTestSrc {
		t.Errorf("rejected invocation must not touch the file (err=%v)", rerr)
	}
}

func TestMissionFmt_RefusesComments(t *testing.T) {
	path := writeSteer(t, strings.Replace(fmtTestSrc, "mission hello_world {", "// the mission\nmission hello_world {", 1))
	_, stderr, err := runMissionFmt(t, path)
	if err == nil {
		t.Fatal("fmt must refuse a commented source")
	}
	if !strings.Contains(stderr, "comments") {
		t.Errorf("refusal should explain the comment limitation, got:\n%s", stderr)
	}
	if got, rerr := os.ReadFile(path); rerr != nil || !strings.Contains(string(got), "// the mission") {
		t.Errorf("refusal must not touch the file (err=%v)", rerr)
	}
}

func TestMissionFmt_ParseErrorExits(t *testing.T) {
	path := writeSteer(t, "mission broken {\n")
	_, stderr, err := runMissionFmt(t, path)
	if err == nil {
		t.Fatal("fmt must fail on a syntax error")
	}
	if !strings.Contains(stderr, "error") {
		t.Errorf("expected a rendered diagnostic, got:\n%s", stderr)
	}
}
