package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/textdiff"
)

// fileChange is the net change a session made to one file: the first
// checkpoint (what it looked like before the run touched it) against the
// file as it is now.
type fileChange struct {
	Path    string
	Workdir string
	Existed bool   // before the run
	Before  string // content before the run ("" when !Existed)
	After   string // current content
	Missing bool   // file no longer exists
	Edits   int    // checkpoints taken for this path
}

// sessionFileChanges collapses a session's checkpoints to one entry per
// path, ordered by first touch.
func sessionFileChanges(app *App, sessionID string) ([]fileChange, error) {
	cps, err := app.Store.ListFileCheckpoints(sessionID)
	if err != nil {
		return nil, err
	}
	first := map[string]*fileChange{}
	var order []string
	for _, cp := range cps {
		key := filepath.Join(cp.Workdir, cp.Path)
		fc, seen := first[key]
		if !seen {
			fc = &fileChange{Path: cp.Path, Workdir: cp.Workdir, Existed: cp.Existed}
			if cp.Existed && cp.BeforeRef != "" {
				var buf bytes.Buffer
				if err := app.Blobs.Get(cp.BeforeRef, &buf); err != nil {
					return nil, fmt.Errorf("read checkpoint for %s: %w", cp.Path, err)
				}
				fc.Before = buf.String()
			}
			first[key] = fc
			order = append(order, key)
		}
		fc.Edits++
	}
	var out []fileChange
	for _, key := range order {
		fc := first[key]
		data, err := os.ReadFile(filepath.Join(fc.Workdir, fc.Path))
		switch {
		case err == nil:
			fc.After = string(data)
		case os.IsNotExist(err):
			fc.Missing = true
		default:
			return nil, err
		}
		out = append(out, *fc)
	}
	return out, nil
}

// resolveCheckpointSession turns "", "latest" or a prefix into a session id
// that has checkpoints.
func resolveCheckpointSession(app *App, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" || arg == "latest" || arg == "last" {
		id, err := app.Store.LatestCheckpointSession()
		if err != nil {
			return "", err
		}
		if id == "" {
			return "", errors.New("no file changes recorded yet (checkpoints are taken when an agent's write_file / edit_file runs through the permissions gate)")
		}
		return id, nil
	}
	return app.Store.ResolveSessionID(arg)
}

// printFileDiffs writes a colored unified diff per changed file.
func printFileDiffs(w io.Writer, changes []fileChange, color bool) {
	paint := func(line string) string {
		if !color {
			return line
		}
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			return ansiBold + line + ansiReset
		case strings.HasPrefix(line, "@@"):
			return ansiCyan + line + ansiReset
		case strings.HasPrefix(line, "+"):
			return ansiGreen + line + ansiReset
		case strings.HasPrefix(line, "-"):
			return ansiRed + line + ansiReset
		}
		return line
	}
	for _, fc := range changes {
		after := fc.After
		if fc.Missing {
			after = ""
		}
		d := textdiff.Unified(fc.Path, fc.Before, after)
		if d == "" {
			fmt.Fprintf(w, "%s: unchanged\n", fc.Path)
			continue
		}
		for _, ln := range strings.Split(strings.TrimRight(d, "\n"), "\n") {
			fmt.Fprintln(w, paint(ln))
		}
	}
}

// undoChanges restores every file to its pre-run state. New files are
// removed, edited files rewritten, deleted files recreated.
func undoChanges(w io.Writer, changes []fileChange, dryRun bool) (restored int, err error) {
	for _, fc := range changes {
		full := filepath.Join(fc.Workdir, fc.Path)
		verb := "restore"
		if !fc.Existed {
			verb = "remove"
		}
		if dryRun {
			fmt.Fprintf(w, "would %s %s\n", verb, fc.Path)
			continue
		}
		if !fc.Existed {
			if rerr := os.Remove(full); rerr != nil && !os.IsNotExist(rerr) {
				return restored, rerr
			}
		} else {
			if merr := os.MkdirAll(filepath.Dir(full), 0o755); merr != nil {
				return restored, merr
			}
			if werr := os.WriteFile(full, []byte(fc.Before), 0o644); werr != nil {
				return restored, werr
			}
		}
		fmt.Fprintf(w, "%sd %s\n", verb, fc.Path)
		restored++
	}
	return restored, nil
}

func newUndoCmd() *cobra.Command {
	var list, dryRun, diff bool
	cmd := &cobra.Command{
		Use:   "undo [session]",
		Short: "roll back the files an agent run changed (default: the latest run with changes)",
		Long: `Every write_file / edit_file an agent runs through the permissions gate
first snapshots the file into uta's content-addressed blob store and
records a checkpoint on the session. uta undo puts every touched file back
to how it was before that run: new files are removed, edited files are
restored. --diff shows what would change; --dry-run lists it; --list shows
the checkpoints themselves.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			arg := ""
			if len(args) == 1 {
				arg = args[0]
			}
			sid, err := resolveCheckpointSession(app, arg)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if list {
				cps, err := app.Store.ListFileCheckpoints(sid)
				if err != nil {
					return err
				}
				for _, cp := range cps {
					state := "existed"
					if !cp.Existed {
						state = "new"
					}
					fmt.Fprintf(out, "%s  %-10s %-8s %s\n", cp.Ts.Format("15:04:05"), cp.Tool, state, cp.Path)
				}
				if len(cps) == 0 {
					fmt.Fprintf(out, "session %s recorded no file checkpoints\n", shortID(sid))
				}
				return nil
			}
			changes, err := sessionFileChanges(app, sid)
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				fmt.Fprintf(out, "session %s changed no files\n", shortID(sid))
				return nil
			}
			if diff {
				noColor, _ := cmd.Flags().GetBool("no-color")
				printFileDiffs(out, changes, !noColor && os.Getenv("NO_COLOR") == "")
				return nil
			}
			sort.SliceStable(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
			n, err := undoChanges(out, changes, dryRun)
			if err != nil {
				return err
			}
			if !dryRun {
				fmt.Fprintf(out, "session %s: %d file(s) restored\n", shortID(sid), n)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&list, "list", false, "list the checkpoints instead of restoring")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be restored")
	cmd.Flags().BoolVar(&diff, "diff", false, "show the diff between the pre-run state and now")
	return cmd
}
