// Package researchdb reads and writes RESEARCH_DATABASE.md, the
// structured ledger the "research" mode uses to record search queries,
// retrieved sources, and the claims those sources support.
//
// RESEARCH_DATABASE.md lives inside the project's shared context
// directory (.uta/context/, the same dir backing $UTA_CONTEXT_DIR). The
// format is YAML frontmatter — last_updated, queries, sources, claims —
// followed by a free-form markdown body the deep-researcher persona uses
// for prose synthesis.
//
// The deep-researcher and citation-curator personas write the ledger
// directly via the worker model's file-IO tools (Read / Edit / Write) —
// the engine does not proxy those calls. To keep persona prompts
// decoupled from filesystem layout, the CLI run loop exports the
// absolute path as the $UTA_RESEARCH_DB env var (alongside
// $UTA_CONTEXT_DIR), so a persona can reference one canonical location
// instead of joining paths itself. The Go-level Load / Write surface is
// reserved for in-process readers like `uta dash`, which renders ledger
// status without shelling out.
package researchdb

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

// Filename is the on-disk name of the research-database document inside
// the project context directory.
const Filename = "RESEARCH_DATABASE.md"

// Source is a single external document the researcher retrieved. URL and
// Retrieved are mandatory — the citation-curator persona refuses to
// emit a claim without both. Title is optional but encouraged so the
// human-readable body can refer to a source by something other than its
// raw URL.
type Source struct {
	URL       string    `yaml:"url"`
	Title     string    `yaml:"title,omitempty"`
	Retrieved time.Time `yaml:"retrieved"`
}

// Claim is one assertion derived from one or more sources. Sources is a
// list of indices into Frontmatter.Sources (1-based to match the way the
// markdown body cites them, e.g. "[1]"). An empty Sources slice is
// rejected on Write so an uncited claim cannot land in the ledger.
type Claim struct {
	Text    string `yaml:"text"`
	Sources []int  `yaml:"sources"`
}

// Frontmatter is the YAML header recognized at the top of a
// RESEARCH_DATABASE.md document.
//
//   - LastUpdated: UTC timestamp of the most recent write. Write stamps
//     this automatically when the caller leaves it zero.
//   - Queries:     the search queries the researcher issued, in
//     invocation order. Useful for re-running an audit
//     trail of what was asked.
//   - Sources:     every distinct external document retrieved across
//     those queries. The ledger is append-friendly — new
//     sources are added at the tail and existing indices
//     remain stable so claims keep pointing at the right
//     documents.
//   - Claims:      structured assertions extracted from the sources.
type Frontmatter struct {
	LastUpdated time.Time `yaml:"last_updated"`
	Queries     []string  `yaml:"queries,omitempty"`
	Sources     []Source  `yaml:"sources,omitempty"`
	Claims      []Claim   `yaml:"claims,omitempty"`
}

// State is a parsed RESEARCH_DATABASE.md document: structured
// frontmatter plus the markdown body underneath.
type State struct {
	Frontmatter Frontmatter
	Body        string
}

// Path returns the absolute path to RESEARCH_DATABASE.md inside
// contextDir. Does NOT verify existence; callers use this to hand the
// location to other tools — the CLI run loop uses it to export
// $UTA_RESEARCH_DB into every subtask's environment.
func Path(contextDir string) string {
	return filepath.Join(contextDir, Filename)
}

// Load reads RESEARCH_DATABASE.md from contextDir. A missing file is not
// an error — callers receive an empty State and can treat it as the
// "fresh start" case. Malformed frontmatter is reported as an error
// rather than silently dropped, so a corrupted file can't be overwritten
// without the operator noticing.
func Load(contextDir string) (*State, error) {
	if contextDir == "" {
		return nil, errors.New("researchdb: contextDir is required")
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

// Write serializes state and atomically replaces RESEARCH_DATABASE.md in
// contextDir. If state.Frontmatter.LastUpdated is the zero value, Write
// stamps it with time.Now().UTC() before serialization so the header is
// always populated.
//
// Validation: every Source must have a non-empty URL and a non-zero
// Retrieved timestamp, and every Claim must cite at least one source by
// a 1-based index inside Frontmatter.Sources. These are the invariants
// the citation-curator persona's charter promises; checking them at the
// writer layer means a future engine-managed integration cannot smuggle
// uncited claims in by accident.
func Write(contextDir string, state *State) error {
	if contextDir == "" {
		return errors.New("researchdb: contextDir is required")
	}
	if state == nil {
		return errors.New("researchdb: state is nil")
	}
	if err := validate(&state.Frontmatter); err != nil {
		return err
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

// validate enforces the source/claim invariants the citation-curator
// charter promises. Called by Write.
func validate(fm *Frontmatter) error {
	for i, s := range fm.Sources {
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("researchdb: sources[%d]: url is required", i)
		}
		if s.Retrieved.IsZero() {
			return fmt.Errorf("researchdb: sources[%d] (%s): retrieved timestamp is required", i, s.URL)
		}
	}
	for i, c := range fm.Claims {
		if strings.TrimSpace(c.Text) == "" {
			return fmt.Errorf("researchdb: claims[%d]: text is required", i)
		}
		if len(c.Sources) == 0 {
			return fmt.Errorf("researchdb: claims[%d]: at least one source citation is required", i)
		}
		for _, idx := range c.Sources {
			if idx < 1 || idx > len(fm.Sources) {
				return fmt.Errorf("researchdb: claims[%d]: source index %d out of range (1..%d)", i, idx, len(fm.Sources))
			}
		}
	}
	return nil
}

// marshal renders state in the canonical
// "---\n<yaml>\n---\n<body>\n" layout. Kept private because callers
// should always go through Write to get atomic writes + the LastUpdated
// default + invariant checks.
func marshal(state *State) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(state.Frontmatter); err != nil {
		return nil, fmt.Errorf("researchdb: marshal frontmatter: %w", err)
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

// parse converts a RESEARCH_DATABASE.md byte stream into a State. A
// document with no leading "---" frontmatter fence is accepted: the
// entire input becomes Body and Frontmatter is zero-valued. This is the
// expected shape the very first time a persona writes to a brand-new
// RESEARCH_DATABASE.md without realizing the canonical layout.
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
		return &State{Body: s}, nil
	}
	fmYAML := rest[:end]
	body := rest[end+len("\n---\n"):]
	body = strings.TrimLeft(body, "\r\n")
	var fm Frontmatter
	if strings.TrimSpace(fmYAML) != "" {
		if err := yaml.Unmarshal([]byte(fmYAML), &fm); err != nil {
			return nil, fmt.Errorf("researchdb: parse frontmatter: %w", err)
		}
	}
	return &State{Frontmatter: fm, Body: body}, nil
}

// AppendEntry merges a new research entry into the existing state: every
// query is appended (duplicates allowed — they record the call log);
// every source is appended only if a source with the same URL is not
// already present, and any returned remap is applied to incoming claim
// citations so the caller doesn't have to track index shifts; every
// claim is appended verbatim with citations remapped to the merged
// source list. Returns the merged State for write-through.
//
// The body argument, if non-empty, is appended to state.Body with a
// blank-line separator. This lets the persona keep a running prose
// synthesis alongside the structured ledger.
func AppendEntry(state *State, queries []string, sources []Source, claims []Claim, body string) *State {
	if state == nil {
		state = &State{}
	}
	state.Frontmatter.Queries = append(state.Frontmatter.Queries, queries...)

	remap := make(map[int]int, len(sources))
	existing := make(map[string]int, len(state.Frontmatter.Sources))
	for i, s := range state.Frontmatter.Sources {
		existing[s.URL] = i + 1
	}
	for i, s := range sources {
		if idx, ok := existing[s.URL]; ok {
			remap[i+1] = idx
			continue
		}
		state.Frontmatter.Sources = append(state.Frontmatter.Sources, s)
		newIdx := len(state.Frontmatter.Sources)
		existing[s.URL] = newIdx
		remap[i+1] = newIdx
	}

	for _, c := range claims {
		remapped := make([]int, 0, len(c.Sources))
		for _, idx := range c.Sources {
			if mapped, ok := remap[idx]; ok {
				remapped = append(remapped, mapped)
			} else {
				remapped = append(remapped, idx)
			}
		}
		state.Frontmatter.Claims = append(state.Frontmatter.Claims, Claim{Text: c.Text, Sources: remapped})
	}

	if body != "" {
		if state.Body != "" && !strings.HasSuffix(state.Body, "\n\n") {
			if strings.HasSuffix(state.Body, "\n") {
				state.Body += "\n"
			} else {
				state.Body += "\n\n"
			}
		}
		state.Body += body
	}
	return state
}
