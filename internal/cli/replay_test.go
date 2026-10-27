package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

func TestReplayCmd_ReplaysEventsToCmdStdout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("UTA_HOME", home)
	t.Chdir(home)

	st, err := store.Open(home + "/uta.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	sessID := "abcd1234-5678-90ef-1234-567890abcdef"
	if err := st.CreateSession(store.Session{
		ID:        sessID,
		Goal:      "test replay goal",
		Worker:    "claude",
		Status:    "completed",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := st.InsertEvent(sessID, "", 1, time.Now(), string(trajectory.GoalReceived), json.RawMessage(`{"goal":"test replay goal"}`)); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	_ = st.Close()

	var stdout, stderr bytes.Buffer
	cmd := newReplayCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"abcd1234", "--delay-ms", "0"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("replay execute: %v (stderr: %s)", err, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Replaying session abcd1234") {
		t.Errorf("stdout missing header: %s", out)
	}
	if !strings.Contains(out, "goal_received") {
		t.Errorf("stdout missing goal_received event: %s", out)
	}
	if !strings.Contains(out, "Replay complete.") {
		t.Errorf("stdout missing completion line: %s", out)
	}
}
