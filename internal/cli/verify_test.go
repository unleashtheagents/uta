package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scriptedVerify returns a verify func that yields the given exit codes in
// order (repeating the last one), recording how many times it ran.
func scriptedVerify(codes ...int) (func(context.Context) (int, string), *int) {
	calls := 0
	f := func(context.Context) (int, string) {
		i := calls
		calls++
		if i >= len(codes) {
			i = len(codes) - 1
		}
		return codes[i], "output-tail"
	}
	return f, &calls
}

func TestVerifyRunner_PassesFirstTry(t *testing.T) {
	verify, vcalls := scriptedVerify(0)
	repairs := 0
	var out strings.Builder
	v := verifyRunner{
		cmdLabel: "go test", maxRepairs: 3, out: &out,
		verify: verify,
		repair: func(context.Context, string) error { repairs++; return nil },
	}
	if !v.run(context.Background()) {
		t.Fatal("should pass")
	}
	if *vcalls != 1 || repairs != 0 {
		t.Errorf("verify=%d repairs=%d, want 1/0", *vcalls, repairs)
	}
	if !strings.Contains(out.String(), "verify passed: go test") {
		t.Errorf("out=%q", out.String())
	}
}

func TestVerifyRunner_RepairsThenPasses(t *testing.T) {
	verify, vcalls := scriptedVerify(1, 1, 0) // fail, fail, pass
	var gotFailures []string
	var out strings.Builder
	v := verifyRunner{
		cmdLabel: "pytest", maxRepairs: 3, out: &out,
		verify: verify,
		repair: func(_ context.Context, failure string) error {
			gotFailures = append(gotFailures, failure)
			return nil
		},
	}
	if !v.run(context.Background()) {
		t.Fatal("should pass after repairs")
	}
	if *vcalls != 3 || len(gotFailures) != 2 {
		t.Errorf("verify=%d repairs=%d, want 3/2", *vcalls, len(gotFailures))
	}
	if gotFailures[0] != "output-tail" {
		t.Errorf("repair should get the failing output tail, got %q", gotFailures[0])
	}
	if !strings.Contains(out.String(), "passed after 2 repair attempt(s)") {
		t.Errorf("out=%q", out.String())
	}
}

func TestVerifyRunner_ExhaustsRepairs(t *testing.T) {
	verify, vcalls := scriptedVerify(1) // always fails
	repairs := 0
	var out strings.Builder
	v := verifyRunner{
		cmdLabel: "make check", maxRepairs: 2, out: &out,
		verify: verify,
		repair: func(context.Context, string) error { repairs++; return nil },
	}
	if v.run(context.Background()) {
		t.Fatal("should fail after exhausting repairs")
	}
	// verify runs maxRepairs+1 times (initial + one after each repair); repair
	// runs exactly maxRepairs times.
	if *vcalls != 3 || repairs != 2 {
		t.Errorf("verify=%d repairs=%d, want 3/2", *vcalls, repairs)
	}
	if !strings.Contains(out.String(), "still failing after 2 repair attempt(s)") {
		t.Errorf("out=%q", out.String())
	}
}

func TestVerifyRunner_ZeroRepairsIsCheckOnly(t *testing.T) {
	verify, vcalls := scriptedVerify(1)
	repairs := 0
	var out strings.Builder
	v := verifyRunner{
		cmdLabel: "lint", maxRepairs: 0, out: &out,
		verify: verify,
		repair: func(context.Context, string) error { repairs++; return nil },
	}
	if v.run(context.Background()) {
		t.Fatal("failing check with no repairs should report failure")
	}
	if *vcalls != 1 || repairs != 0 {
		t.Errorf("verify=%d repairs=%d, want 1/0 (check-only)", *vcalls, repairs)
	}
}

func TestVerifyRunner_RepairInfraFailureStops(t *testing.T) {
	verify, _ := scriptedVerify(1)
	var out strings.Builder
	v := verifyRunner{
		cmdLabel: "go build", maxRepairs: 3, out: &out,
		verify: verify,
		repair: func(context.Context, string) error { return errors.New("provider unreachable") },
	}
	if v.run(context.Background()) {
		t.Fatal("a repair that can't run should end the loop as failure")
	}
	if !strings.Contains(out.String(), "repair turn failed to run: provider unreachable") {
		t.Errorf("out=%q", out.String())
	}
}

func TestVerifyRunner_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	verify, vcalls := scriptedVerify(1)
	v := verifyRunner{
		cmdLabel: "x", maxRepairs: 3, out: &strings.Builder{},
		verify: verify,
		repair: func(context.Context, string) error { return nil },
	}
	if v.run(ctx) {
		t.Fatal("cancelled context should stop with failure")
	}
	if *vcalls != 0 {
		t.Errorf("cancelled before first verify, got %d verify calls", *vcalls)
	}
}

func TestExecVerifyCommand(t *testing.T) {
	dir := t.TempDir()
	// passing command
	if code, _ := execVerifyCommand(context.Background(), "true", dir, 0); code != 0 {
		t.Errorf("`true` exit=%d, want 0", code)
	}
	// failing command with output captured
	code, tail := execVerifyCommand(context.Background(), "echo boom >&2; exit 3", dir, 0)
	if code != 3 {
		t.Errorf("exit=%d, want 3", code)
	}
	if !strings.Contains(tail, "boom") {
		t.Errorf("tail should capture stderr, got %q", tail)
	}
}
