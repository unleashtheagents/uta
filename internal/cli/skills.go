package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/skills"
)

func newSkillsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "discover workspace rules, progressive skills, and subagents (agy, claude, uta)",
		Long: `Discover and inspect project customizations across Google Antigravity (agy),
Anthropic Claude Code (claude), and uta:

  - Hierarchical Rules: AGENTS.md, CLAUDE.md, GEMINI.md, .uta/rules/*.md, .agents/rules/*.md
  - Progressive Skills: skills/<name>/SKILL.md under .uta/, .agents/, .claude/, and global homes
  - Subagents:          .claude/agents/*.md, .uta/agents/*.md, .agents/agents/*.md
  - Lifecycle Hooks:    .uta/hooks.json, .agents/hooks.json

When UTA_NEBIUS_INJECT_RULES=1 is set, discovered rules and progressive skill
summaries are automatically injected into Nebius Token Factory (NVIDIA Nemotron)
sessions.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSkillsList(cmd, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit discovered customizations as JSON")

	var listJSON bool
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "list all discovered rules, skills, subagents, and hooks",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSkillsList(cmd, listJSON)
		},
	}
	listCmd.Flags().BoolVar(&listJSON, "json", false, "emit discovered customizations as JSON")

	showCmd := &cobra.Command{
		Use:   "show <name>",
		Short: "display the full instructions of a discovered skill, rule, or subagent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			home, _ := os.UserHomeDir()
			cat := skills.Discover(cwd, home)
			it, ok := cat.Find(args[0])
			if !ok {
				return fmt.Errorf("skill or rule %q not found in %s", args[0], cwd)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "name:        %s\nkind:        %s\necosystem:   %s\nscope:       %s\ntrigger:     %s\npath:        %s\ndescription: %s\n\n%s\n",
				it.Name, it.Kind, it.Ecosystem, it.Scope, it.Trigger, it.Path, it.Description, it.Body)
			return nil
		},
	}

	cmd.AddCommand(listCmd, showCmd)
	return cmd
}

func runSkillsList(cmd *cobra.Command, asJSON bool) error {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	cat := skills.Discover(cwd, home)
	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(cat)
	}
	out := cmd.OutOrStdout()
	if len(cat.Items) == 0 {
		fmt.Fprintf(out, "No rules or skills discovered in %s\n", cat.Workdir)
		fmt.Fprintln(out, "Tip: add AGENTS.md, CLAUDE.md, GEMINI.md, or .agents/skills/<name>/SKILL.md")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tNAME\tECOSYSTEM\tSCOPE\tTRIGGER\tDESCRIPTION")
	for _, it := range cat.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			it.Kind, it.Name, it.Ecosystem, it.Scope, it.Trigger, truncateLine(it.Description, 64))
	}
	return tw.Flush()
}
