package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/version"
)

var execLookPath = exec.LookPath

// doctorCheck is one line of the diagnosis. Status is one of "ok", "warn",
// "fail", or "--" (informational absence, e.g. an optional tool not on
// PATH). Extra lines render indented beneath the check in text mode and as
// an array in JSON mode.
type doctorCheck struct {
	Status string   `json:"status"`
	Name   string   `json:"name"`
	Detail string   `json:"detail"`
	Extra  []string `json:"extra,omitempty"`
}

// doctorReport is the machine-readable shape `uta doctor --json` emits.
type doctorReport struct {
	Version string        `json:"version"`
	Checks  []doctorCheck `json:"checks"`
	OK      bool          `json:"ok"`
}

func newDoctorCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "verify uta's environment: home dir, db, providers",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON (for CI / scripting)")
	return cmd
}

func runDoctor(cmd *cobra.Command, asJSON bool) error {
	report := doctorReport{Version: version.String(), OK: true}
	add := func(c doctorCheck) {
		if c.Status == "fail" || c.Status == "warn" {
			report.OK = false
		}
		report.Checks = append(report.Checks, c)
	}

	// PATH-shadowing check: when `uta` on PATH is NOT the binary
	// currently running, the user's shell will execute something else
	// than what they think they installed — typically a stale pip/conda
	// entry point from an old prototype, or a leftover dev symlink.
	// That failure mode presents as "uta is broken" with errors that
	// have nothing to do with this binary, so surface it first and
	// loudly.
	if self, resolved, shadowed := pathShadowCheck(); shadowed {
		add(doctorCheck{Status: "fail", Name: "PATH", Detail: fmt.Sprintf("'uta' resolves to %s", resolved), Extra: []string{
			fmt.Sprintf("but this binary is  %s", self),
			"remove the shadowing file (pip uninstall uta / rm it)",
			"or put this binary's directory earlier in PATH",
		}})
	} else if self != "" {
		add(doctorCheck{Status: "ok", Name: "PATH", Detail: self})
	}

	home, err := paths.Home()
	if err != nil {
		add(doctorCheck{Status: "fail", Name: "UTA_HOME", Detail: err.Error()})
		return finishDoctor(cmd, report, asJSON)
	}
	if err := writableCheck(home); err != nil {
		add(doctorCheck{Status: "fail", Name: "UTA_HOME", Detail: fmt.Sprintf("%s (%v)", home, err)})
	} else {
		add(doctorCheck{Status: "ok", Name: "UTA_HOME", Detail: home})
	}

	// Scope conflict: an explicit $UTA_HOME disables project auto-detection
	// (see newApp), so a .uta/ project up the tree is silently ignored.
	// People forget an exported UTA_HOME and then wonder why project state
	// isn't picked up — make the collision visible.
	if os.Getenv("UTA_HOME") != "" {
		cwd, _ := os.Getwd()
		if root, ok := paths.FindProjectRoot(cwd); ok {
			add(doctorCheck{Status: "warn", Name: "scope", Detail: fmt.Sprintf("$UTA_HOME is set, so the project at %s is being IGNORED", root), Extra: []string{
				"unset UTA_HOME to use the project's state dir",
			}})
		}
	}

	dbPath := paths.DB(home)
	st, err := store.Open(dbPath)
	if err != nil {
		add(doctorCheck{Status: "fail", Name: "sqlite", Detail: fmt.Sprintf("%s (%v)", dbPath, err)})
	} else {
		head, _ := store.SchemaHead(st.DB)
		add(doctorCheck{Status: "ok", Name: "sqlite", Detail: fmt.Sprintf("%s (schema %s)", dbPath, head)})
		// Integrity: quick_check catches page-level corruption early —
		// much better surfaced here than as a weird failure mid-run.
		var integrity string
		if qerr := st.DB.QueryRow(`PRAGMA quick_check(1)`).Scan(&integrity); qerr != nil {
			add(doctorCheck{Status: "warn", Name: "db integrity", Detail: fmt.Sprintf("quick_check failed to run: %v", qerr)})
		} else if integrity != "ok" {
			add(doctorCheck{Status: "fail", Name: "db integrity", Detail: fmt.Sprintf("quick_check: %s", integrity), Extra: []string{
				"back up " + dbPath + " and consider `uta exportdb` + reimport into a fresh home",
			}})
		} else {
			add(doctorCheck{Status: "ok", Name: "db integrity", Detail: "quick_check ok"})
		}
		add(blobsCheck(st, home))
		st.Close()
	}

	// Config validation: parse every provider descriptor and persona the
	// way startup does, but surface the per-file errors as first-class
	// findings instead of transient startup warnings.
	provsDir, _ := paths.ProvidersDir(home)
	pc, _ := countEntries(provsDir)
	if _, descErrs := config.LoadProvidersDir(provsDir); len(descErrs) > 0 {
		extra := make([]string, 0, len(descErrs))
		for _, e := range descErrs {
			extra = append(extra, e.Error())
		}
		add(doctorCheck{Status: "warn", Name: "providers dir", Detail: fmt.Sprintf("%s (%d descriptors, %d invalid)", provsDir, pc, len(descErrs)), Extra: extra})
	} else {
		add(doctorCheck{Status: "ok", Name: "providers dir", Detail: fmt.Sprintf("%s (%d descriptors)", provsDir, pc)})
	}
	if personasDir, perr := config.PersonasDir(home); perr == nil {
		if _, personaErrs := config.LoadPersonasDir(personasDir); len(personaErrs) > 0 {
			extra := make([]string, 0, len(personaErrs))
			for _, e := range personaErrs {
				extra = append(extra, e.Error())
			}
			add(doctorCheck{Status: "warn", Name: "personas dir", Detail: fmt.Sprintf("%s (%d invalid)", personasDir, len(personaErrs)), Extra: extra})
		} else {
			add(doctorCheck{Status: "ok", Name: "personas dir", Detail: personasDir})
		}
	}

	app, err := newApp(cmd.Context())
	if err != nil {
		add(doctorCheck{Status: "fail", Name: "app", Detail: err.Error()})
		return finishDoctor(cmd, report, asJSON)
	}
	defer app.Close()

	detections := app.Registry.DetectAll(cmd.Context())
	anyProvider := false
	for _, name := range app.Registry.Names() {
		d := detections[name]
		switch {
		case d.Available:
			caps := capsString(d.Capabilities)
			detail := fmt.Sprintf("%s %s  caps: %s", d.BinaryPath, d.Version, caps)
			if d.Notes != "" {
				detail += "  notes: " + d.Notes
			}
			add(doctorCheck{Status: "ok", Name: "provider: " + name, Detail: detail})
			anyProvider = true
		default:
			notes := d.Notes
			if notes == "" {
				notes = "not detected"
			}
			c := doctorCheck{Status: "--", Name: "provider: " + name, Detail: notes}
			if d.Err != nil {
				c.Extra = []string{fmt.Sprintf("error: %v", d.Err)}
			}
			add(c)
		}
	}
	if !anyProvider {
		add(doctorCheck{Status: "warn", Name: "providers", Detail: "no providers detected. Install at least one of: claude, gemini."})
	}

	// Audit tool adapters — best-effort PATH probe.
	for _, toolID := range engine.KnownBuiltinTools() {
		spec := engine.ResolveToolSpec(engine.ToolSpec{ID: toolID})
		if spec.Cmd == "" {
			continue
		}
		path, err := execLookPath(spec.Cmd)
		if err == nil {
			add(doctorCheck{Status: "ok", Name: "tool: " + toolID, Detail: path})
		} else {
			add(doctorCheck{Status: "--", Name: "tool: " + toolID, Detail: fmt.Sprintf("not on PATH (binary: %s)", spec.Cmd)})
		}
	}

	return finishDoctor(cmd, report, asJSON)
}

// finishDoctor renders the collected report (text or JSON) and maps the
// overall verdict to the exit code contract: 0 healthy, 2 when any check
// failed or warned.
func finishDoctor(cmd *cobra.Command, report doctorReport, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
		if !report.OK {
			return exitWith(2)
		}
		return nil
	}

	fmt.Fprintf(out, "uta %s\n\n", report.Version)
	for _, c := range report.Checks {
		printCheck(out, c)
	}
	fmt.Fprintln(out)
	if report.OK {
		fmt.Fprintln(out, "All systems go. Try:  uta hello  (60-second guided tour)")
		fmt.Fprintln(out, "                  or:  uta run -g \"summarize this repo\" -y")
		return nil
	}
	fmt.Fprintln(out, "Some checks failed or no provider is installed. See lines above.")
	fmt.Fprintln(out, "Tip:  `uta hello` explains what to install and where to read more.")
	return exitWith(2)
}

func printCheck(out io.Writer, c doctorCheck) {
	fmt.Fprintf(out, "%-7s%-19s %s\n", "["+c.Status+"]", c.Name, c.Detail)
	for _, line := range c.Extra {
		fmt.Fprintf(out, "%-7s%-19s %s\n", "", "", line)
	}
}

// blobsCheck reports the blob store's size and how much of it nothing in
// the database references anymore (deleted/imported-over sessions leave
// their content-addressed files behind — the store has no GC).
func blobsCheck(st *store.Store, home string) doctorCheck {
	blobsDir, err := paths.Blobs(home)
	if err != nil {
		return doctorCheck{Status: "fail", Name: "blobs", Detail: err.Error()}
	}
	count, _ := countEntries(blobsDir)
	detail := fmt.Sprintf("%s (%d entries)", blobsDir, count)
	orphans, size, oerr := st.UnreferencedBlobs(store.NewBlobs(blobsDir))
	switch {
	case oerr != nil:
		return doctorCheck{Status: "warn", Name: "blobs", Detail: detail, Extra: []string{
			fmt.Sprintf("orphan scan failed: %v", oerr),
		}}
	case len(orphans) > 0:
		return doctorCheck{Status: "warn", Name: "blobs", Detail: detail, Extra: []string{
			fmt.Sprintf("%d unreferenced blob(s), %s reclaimable — no session/subtask row points at them", len(orphans), humanBytes(size)),
			"safe to delete if you don't plan to re-import an old exportdb dump",
		}}
	default:
		return doctorCheck{Status: "ok", Name: "blobs", Detail: detail}
	}
}

// humanBytes renders a byte count with a binary-ish unit for status lines.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// pathShadowCheck compares the running binary's path with what `uta`
// resolves to on PATH. Returns (selfPath, resolvedPath, shadowed).
// Symlinks are followed on both sides so a brew Cellar binary reached
// via /opt/homebrew/bin/uta does not count as shadowed. Resolution
// failures (no `uta` on PATH at all — e.g. running via ./uta) are not
// shadowing; they report shadowed=false with resolved="".
func pathShadowCheck() (self, resolved string, shadowed bool) {
	selfRaw, err := os.Executable()
	if err != nil {
		return "", "", false
	}
	self = evalSymlinks(selfRaw)
	resolvedRaw, err := execLookPath("uta")
	if err != nil {
		return self, "", false
	}
	resolved = evalSymlinks(resolvedRaw)
	return self, resolved, resolved != self
}

// evalSymlinks resolves a path fully, falling back to the input when
// resolution fails (dangling link, permission) — the comparison then
// happens on raw paths, which is still meaningful.
func evalSymlinks(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func writableCheck(dir string) error {
	f, err := os.CreateTemp(dir, ".uta-write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

func countEntries(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

func capsString(caps []provider.Capability) string {
	if len(caps) == 0 {
		return "-"
	}
	s := make([]string, 0, len(caps))
	for _, c := range caps {
		s = append(s, string(c))
	}
	return strings.Join(s, ",")
}
