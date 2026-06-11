package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// VibeFileSuffix is the on-disk suffix uta uses for per-mode "vibe"
// directives. A mode named "audit" gets a vibe at
// <projectRoot>/.uta/profiles/audit.vibe.md.
const VibeFileSuffix = ".vibe.md"

// VibePath returns the on-disk path for a mode's vibe file, or "" when
// projectRoot is empty (vibes are project-scoped — they steer a mode's
// behavior between runs and don't make sense outside a project).
func VibePath(projectRoot, modeName string) string {
	if projectRoot == "" || modeName == "" {
		return ""
	}
	return filepath.Join(ProjectProfilesDir(projectRoot), modeName+VibeFileSuffix)
}

// ReadVibe returns the full text of a mode's vibe file. A missing file
// is not an error — callers get ("", nil) so they can treat "no vibe"
// uniformly. Whitespace is left untouched so timestamps and headings
// flow into the prompt as written.
func ReadVibe(projectRoot, modeName string) (string, error) {
	path := VibePath(projectRoot, modeName)
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// AppendVibe appends a timestamped entry to a mode's vibe file, creating
// the parent directory and file on demand. The directive is recorded
// verbatim under a `## <RFC3339 UTC timestamp>` heading so a reader can
// scan history and the most recent intent floats to the bottom.
//
// Returns an error when projectRoot is empty or the directive is blank.
func AppendVibe(projectRoot, modeName, directive string) error {
	directive = strings.TrimSpace(directive)
	if directive == "" {
		return errors.New("vibe: directive must not be empty")
	}
	path := VibePath(projectRoot, modeName)
	if path == "" {
		return errors.New("vibe: project root and mode name are required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("vibe: ensure dir: %w", err)
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	entry := fmt.Sprintf("## %s\n%s\n\n", ts, directive)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("vibe: open: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(entry); err != nil {
		return fmt.Errorf("vibe: write: %w", err)
	}
	return nil
}

// ClearVibe removes a mode's vibe file. A missing file is not an error
// (the post-condition — "no vibe" — already holds).
func ClearVibe(projectRoot, modeName string) error {
	path := VibePath(projectRoot, modeName)
	if path == "" {
		return errors.New("vibe: project root and mode name are required")
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("vibe: remove: %w", err)
	}
	return nil
}

// VibePromptSection wraps a vibe's file contents in the `## Current vibe`
// markdown section that gets appended to every persona prompt when the
// mode is active. Returns "" when vibe is blank.
func VibePromptSection(vibe string) string {
	if strings.TrimSpace(vibe) == "" {
		return ""
	}
	return "## Current vibe\n" + strings.TrimRight(vibe, "\n") + "\n"
}
