package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/provider/builtin"
	"github.com/unleashtheagents/uta/internal/skills"
	"github.com/unleashtheagents/uta/internal/tavily"
)

type nebiusStatusReport struct {
	Endpoint      string                    `json:"endpoint"`
	Authenticated bool                      `json:"authenticated"`
	TavilyReady   bool                      `json:"tavily_ready"`
	ActiveModel   string                    `json:"active_model"`
	PlannerModel  string                    `json:"planner_model"`
	WorkerModel   string                    `json:"worker_model"`
	NanoModel     string                    `json:"nano_model"`
	SentinelModel string                    `json:"sentinel_model"`
	VisionModel   string                    `json:"vision_model"`
	RulesCount    int                       `json:"rules_count"`
	SkillsCount   int                       `json:"skills_count"`
	Models        []builtin.NebiusModelSpec `json:"models"`
}

func buildNebiusStatus(workdir, homeDir string) nebiusStatusReport {
	baseURL := strings.TrimSpace(os.Getenv("NEBIUS_BASE_URL"))
	if baseURL == "" {
		baseURL = builtin.DefaultNebiusBaseURL
	}
	activeModel := builtin.ResolveNebiusModel(os.Getenv("NEBIUS_MODEL"))
	cat := skills.Discover(workdir, homeDir)
	return nebiusStatusReport{
		Endpoint:      baseURL,
		Authenticated: strings.TrimSpace(os.Getenv("NEBIUS_API_KEY")) != "",
		TavilyReady:   strings.TrimSpace(os.Getenv("TAVILY_API_KEY")) != "",
		ActiveModel:   activeModel,
		PlannerModel:  builtin.NebiusModelUltra,
		WorkerModel:   builtin.NebiusModelSuper,
		NanoModel:     builtin.NebiusModelNano,
		SentinelModel: builtin.NebiusModelLightning,
		VisionModel:   builtin.NebiusModelSuperVL,
		RulesCount:    len(cat.Rules()),
		SkillsCount:   len(cat.Skills()),
		Models:        builtin.NebiusModelCatalog(),
	}
}

func newNebiusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "nebius",
		Short: "Nebius Token Factory × NVIDIA Nemotron multi-agent coordinator",
		Long: `Inspect and control the Nebius Token Factory × NVIDIA Nemotron agent coordinator.

Coordinates NVIDIA Nemotron-3-Ultra (550B/55B MoE) for architectural planning and
synthesis, Nemotron-3-Super (120B/12B MoE) for 90% parallel worker fan-out,
Nemotron-3-Nano (30B/3B MoE) for rapid context summarization, Nemotron-3.5-Lightning
for ~300 ms OWASP ASI01-ASI10 tool-call sentinel verdicts, and Nemotron-3.5-Super-VL for
multimodal visual analysis — alongside local Claude Code and Google Antigravity (agy) CLIs.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNebiusStatus(cmd, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit coordinator status as JSON")

	var statusJSON bool
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "show Nebius Token Factory readiness, 90/10 routing tiers, and workspace skills",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNebiusStatus(cmd, statusJSON)
		},
	}
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "emit coordinator status as JSON")

	var modelsJSON bool
	modelsCmd := &cobra.Command{
		Use:   "models",
		Short: "list NVIDIA Nemotron models and aliases on Nebius Token Factory",
		RunE: func(cmd *cobra.Command, args []string) error {
			models := builtin.NebiusModelCatalog()
			if modelsJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(models)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ALIAS\tMODEL ID\tPARAMS\tCONTEXT\tSWE-VERIFIED\tROLE")
			for _, m := range models {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					m.Alias, m.ID, m.Params, m.ContextWindow, m.SWEVerified, m.Role)
			}
			return tw.Flush()
		},
	}
	modelsCmd.Flags().BoolVar(&modelsJSON, "json", false, "emit model catalog as JSON")

	var routeJSON bool
	routeCmd := &cobra.Command{
		Use:   "route <goal>",
		Short: "classify a goal into the optimal 90/10 NVIDIA Nemotron model tier",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			goal := strings.Join(args, " ")
			dec := builtin.RouteNebiusGoal(goal)
			if routeJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(dec)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Nebius × NVIDIA Nemotron Coordinator Routing\n")
			fmt.Fprintf(out, "  goal:     %s\n", dec.Goal)
			fmt.Fprintf(out, "  tier:     %s (%s)\n", dec.Tier, dec.Split)
			fmt.Fprintf(out, "  planner:  %s\n", dec.PlannerModel)
			fmt.Fprintf(out, "  worker:   %s\n", dec.WorkerModel)
			fmt.Fprintf(out, "  sentinel: %s\n", dec.SentinelModel)
			fmt.Fprintf(out, "  reason:   %s\n\n", dec.Reason)
			fmt.Fprintf(out, "Run now:\n")
			fmt.Fprintf(out, "  NEBIUS_MODEL=%s uta run -g %q --worker nebius -y\n", dec.WorkerModel, dec.Goal)
			return nil
		},
	}
	routeCmd.Flags().BoolVar(&routeJSON, "json", false, "emit routing decision as JSON")

	var (
		searchMaxResults int
		searchSynthesize bool
		searchJSON       bool
	)
	searchCmd := &cobra.Command{
		Use:   "search <query>",
		Short: "run a live Tavily search and optionally synthesize findings with NVIDIA Nemotron",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()

			tv := &tavily.Client{}
			sr, err := tv.Search(ctx, query, searchMaxResults)
			if err != nil {
				return err
			}

			var synthesis string
			if searchSynthesize && strings.TrimSpace(os.Getenv("NEBIUS_API_KEY")) != "" {
				neb := &builtin.Nebius{}
				prompt := fmt.Sprintf("Synthesize these live Tavily search findings for engineering action:\n\n%s", tavily.FormatMarkdown(sr))
				res, err := neb.RunHeadless(ctx, prompt, provider.RunOptions{
					Env: []string{"NEBIUS_MODEL=" + builtin.NebiusModelSuper},
				}, nil)
				if err == nil {
					synthesis = strings.TrimSpace(res.FinalText)
				}
			}

			// Append to .uta/context/RESEARCH_DATABASE.md if present in workspace.
			if cwd, err := os.Getwd(); err == nil {
				ctxDir := filepath.Join(cwd, ".uta", "context")
				if st, err := os.Stat(ctxDir); err == nil && st.IsDir() {
					dbPath := filepath.Join(ctxDir, "RESEARCH_DATABASE.md")
					f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
					if err == nil {
						_, _ = fmt.Fprintf(f, "\n\n%s\n", tavily.FormatMarkdown(sr))
						if synthesis != "" {
							_, _ = fmt.Fprintf(f, "\n**Nemotron Synthesis:**\n%s\n", synthesis)
						}
						_ = f.Close()
					}
				}
			}

			if searchJSON {
				outObj := map[string]any{
					"tavily":    sr,
					"synthesis": synthesis,
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(outObj)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintln(out, tavily.FormatMarkdown(sr))
			if synthesis != "" {
				fmt.Fprintf(out, "\n### NVIDIA Nemotron Synthesis\n%s\n", synthesis)
			}
			return nil
		},
	}
	searchCmd.Flags().IntVar(&searchMaxResults, "max-results", 5, "maximum number of Tavily search results")
	searchCmd.Flags().BoolVar(&searchSynthesize, "synthesize", true, "synthesize Tavily results with NVIDIA Nemotron when NEBIUS_API_KEY is set")
	searchCmd.Flags().BoolVar(&searchJSON, "json", false, "emit Tavily search results and synthesis as JSON")

	var pingJSON bool
	pingCmd := &cobra.Command{
		Use:   "ping",
		Short: "verify live NEBIUS_API_KEY connectivity against Nebius Token Factory (/v1/models)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			neb := &builtin.Nebius{}
			start := time.Now()
			models, err := neb.FetchModels(ctx)
			elapsed := time.Since(start)
			if err != nil {
				return err
			}
			if pingJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"ok":          true,
					"latency_ms":  elapsed.Milliseconds(),
					"model_count": len(models),
					"models":      models,
				})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Nebius Token Factory ping OK (%d ms) — %d remote model(s) available\n",
				elapsed.Milliseconds(), len(models))
			for i, m := range models {
				if i >= 12 {
					fmt.Fprintf(out, "  ... and %d more\n", len(models)-12)
					break
				}
				fmt.Fprintf(out, "  - %s\n", m)
			}
			return nil
		},
	}
	pingCmd.Flags().BoolVar(&pingJSON, "json", false, "emit ping result as JSON")

	cmd.AddCommand(statusCmd, modelsCmd, routeCmd, searchCmd, pingCmd, newNebiusCostCmd(), newNebiusDemoCmd())
	return cmd
}

func runNebiusStatus(cmd *cobra.Command, asJSON bool) error {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	rep := buildNebiusStatus(cwd, home)
	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	out := cmd.OutOrStdout()
	authLabel := "missing (export NEBIUS_API_KEY=\"v1.nebius...\")"
	if rep.Authenticated {
		authLabel = "ready (NEBIUS_API_KEY configured)"
	}
	tavilyLabel := "optional (export TAVILY_API_KEY=\"tvly-...\" for live web grounding)"
	if rep.TavilyReady {
		tavilyLabel = "ready (TAVILY_API_KEY configured)"
	}
	fmt.Fprintln(out, "Nebius Token Factory × NVIDIA Nemotron Coordinator")
	fmt.Fprintf(out, "  endpoint:       %s\n", rep.Endpoint)
	fmt.Fprintf(out, "  auth status:    %s\n", authLabel)
	fmt.Fprintf(out, "  tavily search:  %s\n", tavilyLabel)
	fmt.Fprintf(out, "  active model:   %s\n", rep.ActiveModel)
	fmt.Fprintf(out, "  planner (10%%):  %s\n", rep.PlannerModel)
	fmt.Fprintf(out, "  workers (90%%):  %s\n", rep.WorkerModel)
	fmt.Fprintf(out, "  fast nano:      %s\n", rep.NanoModel)
	fmt.Fprintf(out, "  sentinel:       %s\n", rep.SentinelModel)
	fmt.Fprintf(out, "  multimodal VL:  %s\n", rep.VisionModel)
	fmt.Fprintln(out, "  routing:        role-tier (planner/synthesis/judge -> ultra, workers -> super, compact -> nano, sentinel -> lightning);")
	fmt.Fprintln(out, "                  NEBIUS_MODEL or [model:<tier>] pins a model, UTA_NEBIUS_ROUTING=0 disables")
	fmt.Fprintf(out, "  customizations: %d rule(s), %d skill(s) discovered (agy / claude / uta)\n\n", rep.RulesCount, rep.SkillsCount)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIER\tMODEL\tPARAMS\tTHROUGHPUT\tROLE")
	for _, m := range rep.Models {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d tok/s\t%s\n",
			m.Alias, m.ID, m.Params, m.ThroughputTokS, m.Role)
	}
	_ = tw.Flush()
	return nil
}
