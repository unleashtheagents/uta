package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/export"
)

func newImportCmd() *cobra.Command {
	var (
		force         bool
		skipHashCheck bool
	)
	cmd := &cobra.Command{
		Use:   "import <file>",
		Short: "restore sessions, subtasks, events, and blobs from a JSON export",
		Long: `Reads a JSON file produced by 'uta exportdb' and writes its contents into
this machine's database and blob store.

By default, importing a session id that already exists is an error. Pass
--force to delete the existing session (and its subtasks + events) first.

Blobs are materialized under ~/.uta/blobs and row references are rewritten
to point at the new paths. The export's body sha256 is verified before any
write happens; pass --skip-hash-check to bypass that.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]

			var r io.Reader
			if path == "-" {
				r = cmd.InOrStdin()
			} else {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				r = f
			}

			var exp export.Export
			if err := json.NewDecoder(r).Decode(&exp); err != nil {
				return fmt.Errorf("parse export: %w", err)
			}

			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			res, err := export.Restore(app.Store, app.Blobs.Dir, &exp, export.ImportOptions{
				Force:         force,
				SkipHashCheck: skipHashCheck,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "imported: sessions=%d subtasks=%d events=%d blobs=%d\n",
				res.SessionsImported, res.SubtasksImported, res.EventsImported, res.BlobsImported)
			if len(res.SessionsReplaced) > 0 {
				fmt.Fprintf(out, "replaced existing sessions: %v\n", res.SessionsReplaced)
			}
			if len(res.MissingBlobRefs) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: %d row(s) reference blobs not present in the export (likely --no-blobs export)\n",
					len(res.MissingBlobRefs))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "replace existing sessions instead of erroring")
	cmd.Flags().BoolVar(&skipHashCheck, "skip-hash-check", false, "skip body_sha256 integrity verification")
	return cmd
}
