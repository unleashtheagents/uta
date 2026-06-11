package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/version"
)

var execLookPath = exec.LookPath

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "verify uta's environment: home dir, db, providers",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd)
		},
	}
}

func runDoctor(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "uta %s\n\n", version.String())

	allOK := true

	home, err := paths.Home()
	if err != nil {
		fmt.Fprintf(out, "[fail] UTA_HOME            %v\n", err)
		return err
	}
	if err := writableCheck(home); err != nil {
		fmt.Fprintf(out, "[fail] UTA_HOME            %s (%v)\n", home, err)
		allOK = false
	} else {
		fmt.Fprintf(out, "[ok]   UTA_HOME            %s\n", home)
	}

	dbPath := paths.DB(home)
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(out, "[fail] sqlite              %s (%v)\n", dbPath, err)
		allOK = false
	} else {
		defer st.Close()
		head, _ := store.SchemaHead(st.DB)
		fmt.Fprintf(out, "[ok]   sqlite              %s (schema %s)\n", dbPath, head)
	}

	blobsDir, err := paths.Blobs(home)
	if err != nil {
		fmt.Fprintf(out, "[fail] blobs               %v\n", err)
		allOK = false
	} else {
		count, _ := countEntries(blobsDir)
		fmt.Fprintf(out, "[ok]   blobs               %s (%d entries)\n", blobsDir, count)
	}

	provsDir, _ := paths.ProvidersDir(home)
	pc, _ := countEntries(provsDir)
	fmt.Fprintf(out, "[ok]   providers dir       %s (%d descriptors)\n", provsDir, pc)

	fmt.Fprintln(out)
	app, err := newApp(cmd.Context())
	if err != nil {
		fmt.Fprintf(out, "[fail] app                 %v\n", err)
		return err
	}
	defer app.Close()

	detections := app.Registry.DetectAll(cmd.Context())
	anyProvider := false
	for _, name := range app.Registry.Names() {
		d := detections[name]
		switch {
		case d.Available:
			caps := capsString(d.Capabilities)
			line := fmt.Sprintf("[ok]   provider: %-9s %s %s  caps: %s", name, d.BinaryPath, d.Version, caps)
			if d.Notes != "" {
				line += "  notes: " + d.Notes
			}
			fmt.Fprintln(out, line)
			anyProvider = true
		default:
			notes := d.Notes
			if notes == "" {
				notes = "not detected"
			}
			fmt.Fprintf(out, "[--]   provider: %-9s %s\n", name, notes)
			if d.Err != nil {
				fmt.Fprintf(out, "       error:               %v\n", d.Err)
			}
		}
	}
	if !anyProvider {
		fmt.Fprintln(out, "\n[warn] no providers detected. Install at least one of: claude, gemini.")
		allOK = false
	}

	// Audit tool adapters — best-effort PATH probe.
	fmt.Fprintln(out)
	for _, toolID := range engine.KnownBuiltinTools() {
		spec := engine.ResolveToolSpec(engine.ToolSpec{ID: toolID})
		if spec.Cmd == "" {
			continue
		}
		path, err := execLookPath(spec.Cmd)
		if err == nil {
			fmt.Fprintf(out, "[ok]   tool:     %-12s %s\n", toolID, path)
		} else {
			fmt.Fprintf(out, "[--]   tool:     %-12s not on PATH (binary: %s)\n", toolID, spec.Cmd)
		}
	}

	fmt.Fprintln(out)
	if allOK && anyProvider {
		fmt.Fprintln(out, "All systems go. Try:  uta hello  (60-second guided tour)")
		fmt.Fprintln(out, "                  or:  uta run -g \"summarize this repo\" -y")
	} else {
		fmt.Fprintln(out, "Some checks failed or no provider is installed. See lines above.")
		fmt.Fprintln(out, "Tip:  `uta hello` explains what to install and where to read more.")
		return exitWith(2)
	}
	return nil
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
