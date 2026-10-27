package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/unleashtheagents/uta/internal/limits"
	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/provider/builtin"
)

// defaultWorker picks the worker for a run that did not name one: Nebius
// Token Factory when a key is configured (the Nemotron tiers give the
// coordinator planner, worker, compact and sentinel roles for the price of
// one key), otherwise the first detected agent CLI.
func defaultWorker(available []string) string {
	for _, n := range available {
		if n == "nebius" {
			return n
		}
	}
	if len(available) > 0 {
		return available[0]
	}
	return ""
}

// isInteractive reports whether both stdin and stdout are terminals.
func isInteractive(in io.Reader, out io.Writer) bool {
	fi, ok := in.(*os.File)
	if !ok || !isatty.IsTerminal(fi.Fd()) {
		return false
	}
	fo, ok := out.(*os.File)
	return ok && isatty.IsTerminal(fo.Fd())
}

// shouldOpenShell decides what a bare `uta` does: with a terminal on both
// ends it opens the shell (after first-run setup when nothing is
// configured); piped or scripted invocations get the help page.
func shouldOpenShell(interactive bool, args []string) bool {
	return interactive && len(args) == 0
}

// setupAnswers is what the wizard collected (or what --flags supplied).
type setupAnswers struct {
	NebiusKey string
	TavilyKey string
	DailyUSD  float64 // 0 keeps the current cap
}

// applySetup persists the answers: keys into credentials.env (0600), the
// daily cap into limits.yaml, and the process env so the current command
// can use them immediately.
func applySetup(home string, a setupAnswers) error {
	credPath := paths.CredentialsFile(home)
	if a.NebiusKey != "" {
		if err := writeCredential(credPath, "NEBIUS_API_KEY", a.NebiusKey, false); err != nil {
			return err
		}
		_ = os.Setenv("NEBIUS_API_KEY", a.NebiusKey)
	}
	if a.TavilyKey != "" {
		if err := writeCredential(credPath, "TAVILY_API_KEY", a.TavilyKey, false); err != nil {
			return err
		}
		_ = os.Setenv("TAVILY_API_KEY", a.TavilyKey)
	}
	if a.DailyUSD > 0 {
		cfg, err := limits.Load(limits.File(home))
		if err != nil {
			return err
		}
		lim := cfg.Providers["nebius"]
		lim.DailyUSD = a.DailyUSD
		cfg.Providers["nebius"] = lim
		if err := limits.Save(limits.File(home), cfg); err != nil {
			return err
		}
	}
	return nil
}

func promptSecret(in io.Reader, errw io.Writer, label string) (string, error) {
	fmt.Fprint(errw, label)
	if f, ok := in.(*os.File); ok && isatty.IsTerminal(f.Fd()) {
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(errw)
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func promptLine(in *bufio.Reader, errw io.Writer, label string) (string, error) {
	fmt.Fprint(errw, label)
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// runSetupWizard asks for what a first run needs: a Nebius Token Factory
// key (Enter skips), an optional Tavily key, and a daily spend cap. It then
// pings Token Factory so the user sees the key work before anything spends.
func runSetupWizard(ctx context.Context, in io.Reader, out, errw io.Writer, home string) error {
	fmt.Fprintln(out, "uta setup — one minute, then agents run.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Nebius Token Factory gives uta five NVIDIA Nemotron tiers behind one key:")
	fmt.Fprintln(out, "  Ultra plans and synthesizes, Super does the work, Nano compacts, Lightning")
	fmt.Fprintln(out, "  guards every tool call. Get a key at https://tokenfactory.nebius.com")
	fmt.Fprintln(out)

	var a setupAnswers
	var err error
	existing := strings.TrimSpace(os.Getenv("NEBIUS_API_KEY"))
	label := "  Nebius API key (input hidden, Enter to skip): "
	if existing != "" {
		label = fmt.Sprintf("  Nebius API key [keep %s] (Enter to keep): ", maskSecret(existing))
	}
	if a.NebiusKey, err = promptSecret(in, errw, label); err != nil {
		return err
	}
	br := bufio.NewReader(in)
	if a.NebiusKey != "" || existing != "" {
		cur := limits.ProviderLimit{}
		if cfg, lerr := limits.Load(limits.File(home)); lerr == nil {
			cur = cfg.Providers["nebius"]
		}
		def := 5.0
		if cur.DailyUSD > 0 {
			def = cur.DailyUSD
		}
		ans, perr := promptLine(br, errw, fmt.Sprintf("  Daily spend cap for nebius in USD [%.2f]: ", def))
		if perr != nil {
			return perr
		}
		if ans == "" {
			a.DailyUSD = def
		} else if a.DailyUSD, err = limits.ParseUSD(ans); err != nil {
			return err
		}
	}
	if a.TavilyKey, err = promptSecret(in, errw, "  Tavily API key for live web grounding (optional, Enter to skip): "); err != nil {
		return err
	}
	if err := applySetup(home, a); err != nil {
		return err
	}

	fmt.Fprintln(out)
	if a.NebiusKey != "" {
		fmt.Fprintf(out, "  stored NEBIUS_API_KEY (%s) in %s\n", maskSecret(a.NebiusKey), paths.CredentialsFile(home))
	}
	if a.TavilyKey != "" {
		fmt.Fprintf(out, "  stored TAVILY_API_KEY (%s)\n", maskSecret(a.TavilyKey))
	}
	if a.DailyUSD > 0 {
		fmt.Fprintf(out, "  nebius spend cap: $%.2f per day (uta limits set nebius ... to change)\n", a.DailyUSD)
	}
	if strings.TrimSpace(os.Getenv("NEBIUS_API_KEY")) != "" {
		pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		start := time.Now()
		models, perr := (&builtin.Nebius{}).FetchModels(pingCtx)
		if perr != nil {
			fmt.Fprintf(out, "  ping: FAILED (%v) — check the key with: uta nebius ping\n", perr)
		} else {
			fmt.Fprintf(out, "  ping: Nebius Token Factory OK in %d ms, %d models available\n", time.Since(start).Milliseconds(), len(models))
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Next: type a goal in the shell, or try `uta nebius demo` for a 60-second tour.")
	return nil
}

// setupMarker records that the first-run hint was shown, so the shell does
// not nag users who deliberately run without Nebius.
func setupMarker(home string) string { return paths.CredentialsFile(home) + ".setup-seen" }

// maybeFirstRunSetup runs the wizard when nothing can run yet (no Nebius key
// and no agent CLI on PATH). When CLIs exist but Nebius is not configured it
// prints a one-time hint instead. Returns true when the wizard ran.
func maybeFirstRunSetup(cmd *cobra.Command, app *App, available []string) (bool, error) {
	hasNebius := strings.TrimSpace(os.Getenv("NEBIUS_API_KEY")) != ""
	if hasNebius {
		return false, nil
	}
	if len(available) == 0 {
		if err := runSetupWizard(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), app.GlobalHome); err != nil {
			return true, err
		}
		return true, nil
	}
	marker := setupMarker(app.GlobalHome)
	if _, err := os.Stat(marker); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "tip: `uta setup` adds Nebius Token Factory — five Nemotron tiers (plan, work, compact, guard) behind one key.")
		_ = os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
	}
	return false, nil
}

func newSetupCmd() *cobra.Command {
	var nebiusKey, tavilyKey, daily string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "one-minute first-run setup: Nebius Token Factory key, daily spend cap, optional Tavily key",
		Long: `Interactive first-run setup. Stores keys in ~/.uta/credentials.env (0600,
outside every repository), sets a daily spend cap for nebius in
~/.uta/limits.yaml, and pings Nebius Token Factory so you see the key work
before anything spends. A bare 'uta' in a terminal runs this automatically
when no provider is configured.

Non-interactive: uta setup --nebius-key "$KEY" --daily-usd 5 [--tavily-key "$TK"]`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := paths.Home()
			if err != nil {
				return err
			}
			if nebiusKey != "" || tavilyKey != "" || daily != "" {
				a := setupAnswers{NebiusKey: strings.TrimSpace(nebiusKey), TavilyKey: strings.TrimSpace(tavilyKey)}
				if daily != "" {
					if a.DailyUSD, err = limits.ParseUSD(daily); err != nil {
						return err
					}
				}
				if err := applySetup(home, a); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "setup saved (%s, %s)\n", paths.CredentialsFile(home), limits.File(home))
				return nil
			}
			return runSetupWizard(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), home)
		},
	}
	cmd.Flags().StringVar(&nebiusKey, "nebius-key", "", "Nebius Token Factory API key (non-interactive)")
	cmd.Flags().StringVar(&tavilyKey, "tavily-key", "", "Tavily API key (non-interactive)")
	cmd.Flags().StringVar(&daily, "daily-usd", "", "daily USD spend cap for nebius (non-interactive)")
	return cmd
}
