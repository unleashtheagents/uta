package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newCtxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ctx",
		Short: "manage the project's shared context store",
		Long: `Project context is a per-project directory (.uta/context/) holding named
files that subtasks in a workflow can read and write. Useful for passing
artifacts between phases — e.g., the contracts phase writes abis.json into
context, the frontend phase reads it.

Inside a running subtask the path is $UTA_CONTEXT_DIR (set automatically).

Subcommands:
  put <name> <file>   stash a file (overwrites any prior entry with that name)
  get <name>          print a context blob to stdout
  list                show every named context blob with size + mtime
  rm <name>           remove a context blob`,
	}
	cmd.AddCommand(newCtxPutCmd())
	cmd.AddCommand(newCtxGetCmd())
	cmd.AddCommand(newCtxListCmd())
	cmd.AddCommand(newCtxRmCmd())
	return cmd
}

func requireProject(app *App) error {
	if !app.InProject() {
		return errors.New("not inside a project. Run 'uta project init' first.")
	}
	return nil
}

func sanitizeCtxName(name string) (string, error) {
	if name == "" {
		return "", errors.New("ctx name is required")
	}
	if filepath.Base(name) != name {
		return "", fmt.Errorf("ctx name must not contain path separators: %q", name)
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("invalid ctx name: %q", name)
	}
	return name, nil
}

func newCtxPutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "put <name> <file>",
		Short: "stash a file as a named context blob (use '-' to read from stdin)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			if err := requireProject(app); err != nil {
				return err
			}
			name, err := sanitizeCtxName(args[0])
			if err != nil {
				return err
			}
			src := args[1]

			var data []byte
			if src == "-" {
				data, err = io.ReadAll(cmd.InOrStdin())
			} else {
				data, err = os.ReadFile(src)
			}
			if err != nil {
				return err
			}
			dest := filepath.Join(app.ContextDir, name)
			if err := os.WriteFile(dest, data, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s (%d bytes)\n", dest, len(data))
			return nil
		},
	}
	return cmd
}

func newCtxGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "print a context blob to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			if err := requireProject(app); err != nil {
				return err
			}
			name, err := sanitizeCtxName(args[0])
			if err != nil {
				return err
			}
			data, err := os.ReadFile(filepath.Join(app.ContextDir, name))
			if err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("ctx %q not found", name)
				}
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
	return cmd
}

func newCtxRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "remove a context blob",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			if err := requireProject(app); err != nil {
				return err
			}
			name, err := sanitizeCtxName(args[0])
			if err != nil {
				return err
			}
			path := filepath.Join(app.ContextDir, name)
			if err := os.Remove(path); err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("ctx %q not found", name)
				}
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "removed", path)
			return nil
		},
	}
	return cmd
}

func newCtxListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list named context blobs",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			if err := requireProject(app); err != nil {
				return err
			}
			entries, err := os.ReadDir(app.ContextDir)
			if err != nil {
				return err
			}
			type row struct {
				Name     string    `json:"name"`
				Size     int64     `json:"size"`
				Modified time.Time `json:"modified"`
				Path     string    `json:"path"`
			}
			var rows []row
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				rows = append(rows, row{
					Name:     e.Name(),
					Size:     info.Size(),
					Modified: info.ModTime(),
					Path:     filepath.Join(app.ContextDir, e.Name()),
				})
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSIZE\tMODIFIED")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%d\t%s\n", r.Name, r.Size, r.Modified.Format(time.RFC3339))
			}
			if len(rows) == 0 {
				fmt.Fprintln(tw, "(empty)")
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}
