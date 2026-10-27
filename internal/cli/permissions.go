package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/policy"
)

func newPermissionsCmd() *cobra.Command {
	var global bool
	cmd := &cobra.Command{
		Use:   "permissions",
		Short: "allow, ask or deny rules for the tools agents run (.uta/permissions.yaml)",
		Long: `Rules for the tools uta executes itself (the nebius worker's read_file,
list_dir, search_files, glob, tavily_search, tavily_extract, write_file,
edit_file and run_command).

  <workdir>/.uta/permissions.yaml   this repository
  ~/.uta/permissions.yaml           everywhere (--global)

Pattern grammar:  tool            every call of the tool
                  tool(glob)      calls whose command/path/query matches
deny beats ask, ask beats allow. With no matching rule, read-only tools run
and write_file / edit_file / run_command ask. At the prompt, [a]lways saves
an allow rule here; [s] approves the tool for the rest of the run.

Hooks (pre_tool / post_tool) run shell commands around tool calls; exit 2 in
a pre hook denies the call, exit 2 in a post hook sends the hook's output
back to the model. See 'uta permissions init' for a commented template.`,
	}
	target := func() (string, error) {
		home, err := paths.Home()
		if err != nil {
			return "", err
		}
		if global {
			return policy.GlobalFile(home), nil
		}
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return policy.ProjectFile(cwd), nil
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "show the effective rules for the current directory and where they come from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := paths.Home()
			if err != nil {
				return err
			}
			cwd, _ := os.Getwd()
			p, err := policy.LoadMerged(home, cwd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(p.Sources) == 0 {
				fmt.Fprintln(out, "no permissions files yet — defaults apply: read-only tools run, write_file / edit_file / run_command ask.")
				fmt.Fprintf(out, "create one with: uta permissions init   (%s)\n", policy.ProjectFile(cwd))
				return nil
			}
			fmt.Fprintf(out, "sources: %s\n", strings.Join(p.Sources, ", "))
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "VERDICT\tPATTERN")
			for _, r := range p.Deny {
				fmt.Fprintf(tw, "deny\t%s\n", r)
			}
			for _, r := range p.Ask {
				fmt.Fprintf(tw, "ask\t%s\n", r)
			}
			for _, r := range p.Allow {
				fmt.Fprintf(tw, "allow\t%s\n", r)
			}
			tw.Flush()
			if n := len(p.Hooks.PreTool) + len(p.Hooks.PostTool); n > 0 {
				fmt.Fprintf(out, "hooks: %d pre_tool, %d post_tool\n", len(p.Hooks.PreTool), len(p.Hooks.PostTool))
			}
			fmt.Fprintln(out, "unmatched: read-only tools allow; write_file, edit_file, run_command ask")
			return nil
		},
	}
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "write a commented starter permissions.yaml (project, or --global)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := target()
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists", path)
			}
			if err := os.MkdirAll(strings.TrimSuffix(path, "/permissions.yaml"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(policy.Template), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
			return nil
		},
	}
	addRule := func(name string, v policy.Verdict, short string) *cobra.Command {
		return &cobra.Command{
			Use:   name + " <pattern>",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				path, err := target()
				if err != nil {
					return err
				}
				if err := policy.AddRule(path, v, args[0]); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s  (%s)\n", v, args[0], path)
				return nil
			},
		}
	}
	cmd.PersistentFlags().BoolVar(&global, "global", false, "edit ~/.uta/permissions.yaml instead of the project file")
	cmd.AddCommand(status, initCmd,
		addRule("allow", policy.Allow, "add an allow rule, e.g. 'run_command(go test *)'"),
		addRule("ask", policy.Ask, "add an ask rule, e.g. 'run_command(git push *)'"),
		addRule("deny", policy.Deny, "add a deny rule, e.g. 'write_file(.env*)'"),
	)
	cmd.RunE = status.RunE
	cmd.Args = cobra.NoArgs
	return cmd
}
