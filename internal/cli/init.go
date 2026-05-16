package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/paths"
)

func newInitCmd() *cobra.Command {
	var workflow bool
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "scaffold example config files",
		Long: `Without flags, prints next-step hints.

--workflow writes an example uta.yaml in the current directory.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if workflow {
				dest := "uta.yaml"
				if _, err := os.Stat(dest); err == nil && !force {
					return fmt.Errorf("%s already exists (pass --force to overwrite)", dest)
				}
				if err := os.WriteFile(dest, []byte(config.ExampleWorkflowYAML), 0o644); err != nil {
					return err
				}
				fmt.Fprintf(out, "wrote %s — edit it, then run:  uta run -f %s -y\n", dest, dest)
				return nil
			}
			home, err := paths.Home()
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "uta home: %s\n", home)
			fmt.Fprintf(out, "providers dir: %s (drop *.yaml here to register a custom agent)\n", filepath.Join(home, "providers"))
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "next steps:")
			fmt.Fprintln(out, "  uta doctor                  # verify environment")
			fmt.Fprintln(out, "  uta init --workflow         # write an example uta.yaml here")
			fmt.Fprintln(out, "  uta run -g \"...\" -y         # ad-hoc run")
			return nil
		},
	}
	cmd.Flags().BoolVar(&workflow, "workflow", false, "write an example uta.yaml in the current directory")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite uta.yaml if it exists")
	return cmd
}

var _ = errors.New
