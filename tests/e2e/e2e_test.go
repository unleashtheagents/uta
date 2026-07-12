//go:build e2e

// Package e2e drives the real uta binary end-to-end against a stub
// provider — a shell script wrapped in a declarative descriptor — so the
// whole surface (cobra wiring, engine, store, steer interpreter, exit
// codes) is exercised the way a user exercises it, with zero API spend.
//
// Run with: CGO_ENABLED=0 go test -tags e2e ./tests/e2e/
package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var (
	utaBin  string
	utaHome string
)

func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		// The stub provider is a POSIX shell script; the Go-level unit
		// tests cover Windows-relevant logic.
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "uta-e2e-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	utaBin = filepath.Join(dir, "uta")
	build := exec.Command("go", "build", "-o", utaBin, "github.com/unleashtheagents/uta/cmd/uta")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		panic("build uta: " + err.Error() + "\n" + string(out))
	}

	// Isolated UTA_HOME with one stub provider: echoes a canned answer,
	// exits 0 for the --version detection probe.
	utaHome = filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(utaHome, "providers"), 0o755); err != nil {
		panic(err)
	}
	stub := filepath.Join(dir, "stub-agent.sh")
	stubScript := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "stub 1.0.0"; exit 0; fi
cat >/dev/null
echo "STUB_ANSWER"
`
	if err := os.WriteFile(stub, []byte(stubScript), 0o755); err != nil {
		panic(err)
	}
	descriptor := `name: stub
binary: ` + stub + `
notes: e2e stub provider
invocation:
  argv: ["run"]
  stdin: "{{prompt}}"
output:
  format: text
`
	if err := os.WriteFile(filepath.Join(utaHome, "providers", "stub.yaml"), []byte(descriptor), 0o644); err != nil {
		panic(err)
	}

	os.Exit(m.Run())
}

// uta runs the built binary with the isolated home. Returns stdout,
// stderr, and the exit code.
func uta(t *testing.T, workdir string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(utaBin, args...)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "UTA_HOME="+utaHome, "NO_COLOR=1")
	var out, errw bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errw
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("uta %v: %v", args, err)
	}
	return out.String(), errw.String(), code
}

const e2eSteer = `agent fn greet(name: Text) -> Text
  worker stub
  prompt """
  Say hello to ${name}.
  """

mission e2e_hello {
  budget 10k tokens
  emit greet("world")
}
`

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestE2E_ProvidersDetectsStub(t *testing.T) {
	out, errw, code := uta(t, t.TempDir(), "providers")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errw)
	}
	if !strings.Contains(out, "stub") {
		t.Errorf("providers output should list the stub:\n%s", out)
	}
}

func TestE2E_MissionCheckAndRun(t *testing.T) {
	dir := t.TempDir()
	prog := writeFile(t, dir, "hello.steer", e2eSteer)

	out, errw, code := uta(t, dir, "mission", "check", prog)
	if code != 0 || !strings.Contains(out, `ok: mission "e2e_hello"`) {
		t.Fatalf("check: exit=%d out=%q err=%q", code, out, errw)
	}

	out, errw, code = uta(t, dir, "mission", "run", prog)
	if code != 0 {
		t.Fatalf("run: exit=%d stderr=%s", code, errw)
	}
	if !strings.Contains(out, "STUB_ANSWER") {
		t.Errorf("final answer missing from stdout:\n%s", out)
	}
	if !strings.Contains(errw, "status=completed") {
		t.Errorf("status line missing from stderr:\n%s", errw)
	}
}

func TestE2E_MissionCheckBadProgramExits2(t *testing.T) {
	dir := t.TempDir()
	prog := writeFile(t, dir, "bad.steer", strings.Replace(e2eSteer, `greet("world")`, `gret("world")`, 1))
	_, errw, code := uta(t, dir, "mission", "check", prog)
	if code != 2 {
		t.Fatalf("exit=%d, want 2; stderr=%s", code, errw)
	}
	if !strings.Contains(errw, `did you mean "greet"`) {
		t.Errorf("diagnostic missing suggestion:\n%s", errw)
	}
}

func TestE2E_MissionRecordedInSessions(t *testing.T) {
	dir := t.TempDir()
	prog := writeFile(t, dir, "hello.steer", e2eSteer)
	if _, errw, code := uta(t, dir, "mission", "run", prog); code != 0 {
		t.Fatalf("run: exit=%d stderr=%s", code, errw)
	}
	out, errw, code := uta(t, dir, "sessions", "--last")
	if code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("sessions --last: exit=%d out=%q err=%q", code, out, errw)
	}
	id := strings.TrimSpace(out)
	out, errw, code = uta(t, dir, "trajectory", id)
	if code != 0 {
		t.Fatalf("trajectory: exit=%d stderr=%s", code, errw)
	}
	if !strings.Contains(out, "mission") {
		t.Errorf("trajectory should carry the mission events:\n%s", out)
	}
}

func TestE2E_RunFallbackPathCompletes(t *testing.T) {
	// The stub cannot produce a parseable plan, so `uta run` exercises the
	// planner-fallback path: one subtask, synthesized answer, exit 0.
	out, errw, code := uta(t, t.TempDir(), "run", "-g", "say hi", "--worker", "stub", "-y", "--timeout", "60s")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errw)
	}
	if !strings.Contains(out, "STUB_ANSWER") {
		t.Errorf("answer missing:\n%s", out)
	}
}

func TestE2E_ShellScriptedSession(t *testing.T) {
	dir := t.TempDir()
	prog := writeFile(t, dir, "hello.steer", e2eSteer)

	cmd := exec.Command(utaBin, "shell", "--worker", "stub")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "UTA_HOME="+utaHome, "NO_COLOR=1")
	cmd.Stdin = strings.NewReader("/status\n/mission check " + prog + "\n/exit\n")
	var out, errw bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errw
	if err := cmd.Run(); err != nil {
		t.Fatalf("shell: %v\nstderr=%s", err, errw.String())
	}
	if !strings.Contains(out.String(), "worker:  stub") {
		t.Errorf("/status output missing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `ok: mission "e2e_hello"`) {
		t.Errorf("/mission check output missing:\n%s", out.String())
	}
}
