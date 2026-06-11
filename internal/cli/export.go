package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/export"
)

func newExportDBCmd() *cobra.Command {
	var (
		all      bool
		noBlobs  bool
		outFile  string
		indented bool
		noRedact bool
	)
	cmd := &cobra.Command{
		Use:   "exportdb [session-id]",
		Short: "dump sessions, subtasks, events, and blobs to a self-contained JSON",
		Long: `Without flags, exports one session (the [session-id] argument). With --all,
exports every session in the store.

By default, every blob referenced by the exported rows is inlined as base64
under the "blobs" key, making the JSON a portable, self-contained artifact.
Pass --no-blobs for a metadata-only export (smaller, but rows will point at
basenames the importer can't materialize).

Output goes to stdout unless -o/--output is set. Header includes
format_version, uta_version, schema_version, exported_at, row counts, and a
sha256 hash of the body — uta import verifies all of these.

Secret-shaped strings (API keys, tokens, private-key blocks, env-style
SECRET/PASSWORD assignments) are scrubbed by default — trajectories record
raw provider output, which can contain material you never intended to
share. The header records what was removed (header.redactions). Pass
--no-redact for a byte-faithful local backup that you do NOT intend to
share.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !all && len(args) == 0 {
				return errors.New("either pass a session-id or --all")
			}
			if all && len(args) > 0 {
				return errors.New("--all and [session-id] are mutually exclusive")
			}

			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			opts := export.Options{
				IncludeBlobs: !noBlobs,
				All:          all,
				NoRedact:     noRedact,
			}
			if !all {
				// Accept the short ids `uta sessions` prints.
				resolved, rerr := app.Store.ResolveSessionID(args[0])
				if rerr != nil {
					return rerr
				}
				opts.SessionID = resolved
			}

			exp, err := export.Run(app.Store, app.Blobs.Dir, opts)
			if err != nil {
				return err
			}

			var w io.Writer = cmd.OutOrStdout()
			var tmp *os.File
			if outFile != "" {
				dir := filepath.Dir(outFile)
				base := filepath.Base(outFile)
				tmp, err = os.CreateTemp(dir, "."+base+".*.tmp")
				if err != nil {
					return err
				}
				w = tmp
			}
			enc := json.NewEncoder(w)
			if indented {
				enc.SetIndent("", "  ")
			}
			if err := enc.Encode(exp); err != nil {
				if tmp != nil {
					tmp.Close()
					os.Remove(tmp.Name())
				}
				return err
			}

			if outFile != "" {
				if err := tmp.Close(); err != nil {
					os.Remove(tmp.Name())
					return err
				}
				if err := os.Rename(tmp.Name(), outFile); err != nil {
					os.Remove(tmp.Name())
					return err
				}
				rc := exp.Header.RowCounts
				fmt.Fprintf(cmd.ErrOrStderr(),
					"wrote %s (sessions=%d subtasks=%d events=%d blobs=%d body_sha256=%s)\n",
					outFile, rc["sessions"], rc["subtasks"], rc["events"], rc["blobs"],
					exp.Header.BodySHA256[:16])
			}
			reportRedactions(cmd.ErrOrStderr(), exp.Header.Redacted, exp.Header.Redactions, noRedact)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "export every session in the store")
	cmd.Flags().BoolVar(&noBlobs, "no-blobs", false, "skip inlining blob contents (smaller, less portable)")
	cmd.Flags().StringVarP(&outFile, "output", "o", "", "write to this file instead of stdout")
	cmd.Flags().BoolVar(&indented, "indent", false, "pretty-print the JSON")
	cmd.Flags().BoolVar(&noRedact, "no-redact", false, "skip secret scrubbing (local backups only — do not share unredacted exports)")
	return cmd
}

// reportRedactions prints a one-line summary of the scrubbing pass to
// stderr. Three states: opt-out (loud warning), nothing found (quiet
// confirmation), or N replacements (per-rule counts so the operator can
// audit what was removed before sharing).
func reportRedactions(w io.Writer, redacted bool, counts map[string]int, optedOut bool) {
	if optedOut {
		fmt.Fprintln(w, "warn: --no-redact set — export may contain secrets; do not share")
		return
	}
	if !redacted {
		return
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		fmt.Fprintln(w, "redaction: no secret-shaped strings found")
		return
	}
	fmt.Fprintf(w, "redaction: %d replacement(s):", total)
	for rule, c := range counts {
		fmt.Fprintf(w, " %s=%d", rule, c)
	}
	fmt.Fprintln(w)
}
