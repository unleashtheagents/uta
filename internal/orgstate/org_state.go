// Package orgstate reads and writes ORG_STATE.md, the structured
// memory document the "ops" mode uses to track the organization's
// commitments, source emails, and pending decisions across runs.
//
// ORG_STATE.md lives inside the project's shared context directory
// (.uta/context/, the same dir backing $UTA_CONTEXT_DIR). The format
// is YAML frontmatter — last_updated, source_emails, pending_decisions —
// followed by a free-form markdown body the comms persona uses for
// human-readable notes.
//
// The comms-agent persona writes ORG_STATE.md directly via the worker
// model's file-IO tools (Read / Edit / Write) — the engine does not
// proxy those calls. To keep persona prompts decoupled from filesystem
// layout, the CLI run loop exports the absolute path as the
// $UTA_ORG_STATE env var (alongside $UTA_CONTEXT_DIR), so a persona
// can reference one canonical location instead of joining paths
// itself. The Go-level Load / Write surface is reserved for in-process
// readers like `uta dash`, which renders ORG_STATE.md status without
// shelling out.
package orgstate

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/unleashtheagents/uta/internal/paths"
)

// Filename is the on-disk name of the org-state document inside the
// project context directory.
const Filename = "ORG_STATE.md"

// Frontmatter is the YAML header recognized at the top of an
// ORG_STATE.md document.
//
//   - LastUpdated:      UTC timestamp of the most recent write. WriteFile
//     stamps this automatically when the caller leaves
//     it zero.
//   - SourceEmails:     IDs / message refs of inbox items that informed
//     the current state. Free-form strings; the comms
//     persona usually populates them with Gmail
//     message IDs.
//   - PendingDecisions: short human-readable items the operator still
//     owes a call on. The comms persona must NOT
//     publish anything that resolves a pending
//     decision without explicit approval.
type Frontmatter struct {
	LastUpdated      time.Time `yaml:"last_updated"`
	SourceEmails     []string  `yaml:"source_emails,omitempty"`
	PendingDecisions []string  `yaml:"pending_decisions,omitempty"`
}

// State is a parsed ORG_STATE.md document: structured frontmatter plus
// the markdown body underneath.
type State struct {
	Frontmatter Frontmatter
	Body        string
}

// Path returns the absolute path to ORG_STATE.md inside contextDir.
// Does NOT verify existence; callers use this to hand the location to
// other tools (e.g. as $UTA_ORG_STATE in a worker prompt).
func Path(contextDir string) string {
	return filepath.Join(contextDir, Filename)
}

// Load reads ORG_STATE.md from contextDir. A missing file is not an
// error — callers receive an empty State and can treat it as the
// "fresh start" case. Malformed frontmatter is reported as an error
// rather than silently dropped, so a corrupted file can't be
// overwritten without the operator noticing.
func Load(contextDir string) (*State, error) {
	if contextDir == "" {
		return nil, errors.New("orgstate: contextDir is required")
	}
	data, err := os.ReadFile(Path(contextDir))
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, err
	}
	return parse(data)
}

// Write serializes state and atomically replaces ORG_STATE.md in
// contextDir. If state.Frontmatter.LastUpdated is the zero value,
// Write stamps it with time.Now().UTC() before serialization so the
// header is always populated.
func Write(contextDir string, state *State) error {
	if contextDir == "" {
		return errors.New("orgstate: contextDir is required")
	}
	if state == nil {
		return errors.New("orgstate: state is nil")
	}
	if state.Frontmatter.LastUpdated.IsZero() {
		state.Frontmatter.LastUpdated = time.Now().UTC()
	}
	buf, err := marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		return err
	}
	return paths.WriteFileAtomic(Path(contextDir), buf, 0o644)
}

// marshal renders state in the canonical
// "---\n<yaml>\n---\n<body>\n" layout. Kept private because callers
// should always go through Write to get atomic writes + the
// LastUpdated default.
func marshal(state *State) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(state.Frontmatter); err != nil {
		return nil, fmt.Errorf("orgstate: marshal frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	buf.WriteString("---\n")
	body := strings.TrimRight(state.Body, "\n")
	if body != "" {
		buf.WriteString(body)
		buf.WriteString("\n")
	}
	return buf.Bytes(), nil
}

// parse converts an ORG_STATE.md byte stream into a State. A document
// with no leading "---" frontmatter fence is accepted: the entire
// input becomes Body and Frontmatter is zero-valued. This is the
// expected shape the very first time a comms persona writes to a
// brand-new ORG_STATE.md without realizing the canonical layout.
func parse(data []byte) (*State, error) {
	s := string(data)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return &State{Body: s}, nil
	}
	var rest string
	switch {
	case strings.HasPrefix(s, "---\r\n"):
		rest = s[len("---\r\n"):]
	default:
		rest = s[len("---\n"):]
	}
	// Closing fence: a line containing only "---". Accept either
	// "\n---\n" or "\n---\r\n", and tolerate a trailing-only "\n---"
	// at EOF (no body).
	end := -1
	for _, sep := range []string{"\n---\n", "\n---\r\n"} {
		if i := strings.Index(rest, sep); i >= 0 {
			end = i
			rest = rest[:i] + "\n---\n" + rest[i+len(sep):]
			break
		}
	}
	if end < 0 {
		if strings.HasSuffix(rest, "\n---") {
			end = len(rest) - len("\n---")
			rest = rest[:end] + "\n---\n"
		}
	}
	if end < 0 {
		// No closing fence — treat the whole input as body. Don't
		// silently swallow it as malformed frontmatter.
		return &State{Body: s}, nil
	}
	fmYAML := rest[:end]
	body := rest[end+len("\n---\n"):]
	body = strings.TrimLeft(body, "\r\n")
	var fm Frontmatter
	if strings.TrimSpace(fmYAML) != "" {
		if err := yaml.Unmarshal([]byte(fmYAML), &fm); err != nil {
			return nil, fmt.Errorf("orgstate: parse frontmatter: %w", err)
		}
	}
	return &State{Frontmatter: fm, Body: body}, nil
}
