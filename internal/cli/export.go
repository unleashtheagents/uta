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
sha256 hash of the body — uta import verifies all of these.`,
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
			}
			if !all {
				opts.SessionID = args[0]
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
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "export every session in the store")
	cmd.Flags().BoolVar(&noBlobs, "no-blobs", false, "skip inlining blob contents (smaller, less portable)")
	cmd.Flags().StringVarP(&outFile, "output", "o", "", "write to this file instead of stdout")
	cmd.Flags().BoolVar(&indented, "indent", false, "pretty-print the JSON")
	return cmd
}
