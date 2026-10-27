package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/unleashtheagents/uta/internal/paths"
)

// credentialVars maps a provider or service name to the environment variable
// uta and its providers read. Bare variable names are accepted too, so
// `uta auth set MY_PROXY_TOKEN` works for declarative providers.
var credentialVars = map[string]string{
	"nebius":    "NEBIUS_API_KEY",
	"tavily":    "TAVILY_API_KEY",
	"anthropic": "ANTHROPIC_API_KEY",
	"claude":    "ANTHROPIC_API_KEY",
	"gemini":    "GEMINI_API_KEY",
	"google":    "GEMINI_API_KEY",
	"openai":    "OPENAI_API_KEY",
}

// credentialVarFor resolves a provider alias or raw variable name.
func credentialVarFor(name string) (string, error) {
	n := strings.TrimSpace(name)
	if v, ok := credentialVars[strings.ToLower(n)]; ok {
		return v, nil
	}
	if n == "" || strings.ContainsAny(n, "= \t\n") || strings.ToUpper(n) != n {
		return "", fmt.Errorf("unknown provider %q (known: %s) — or pass an UPPER_CASE variable name", name, strings.Join(knownCredentialNames(), ", "))
	}
	return n, nil
}

func knownCredentialNames() []string {
	out := make([]string, 0, len(credentialVars))
	for k := range credentialVars {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// readCredentialsFile parses ~/.uta/credentials.env into ordered keys and a
// map, preserving comment lines so a rewrite keeps the user's notes.
func readCredentialsFile(path string) (lines []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{
				"# ~/.uta/credentials.env — user-global API credentials for uta (chmod 600).",
				"# Stored outside every git repository. Auto-loaded by uta on startup.",
				"# Manage with `uta auth set|unset|status`.",
			}, nil
		}
		return nil, err
	}
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		lines = append(lines, l)
	}
	return lines, nil
}

func credentialLineKey(line string) string {
	l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
	if l == "" || strings.HasPrefix(l, "#") {
		return ""
	}
	k, _, ok := strings.Cut(l, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(k)
}

// writeCredential sets or removes one variable in the credentials file,
// atomically, with 0600 permissions.
func writeCredential(path, key, value string, remove bool) error {
	lines, err := readCredentialsFile(path)
	if err != nil {
		return err
	}
	var out []string
	replaced := false
	for _, l := range lines {
		if credentialLineKey(l) == key {
			if remove {
				continue
			}
			if !replaced {
				out = append(out, fmt.Sprintf("export %s=%q", key, value))
				replaced = true
			}
			continue
		}
		out = append(out, l)
	}
	if !remove && !replaced {
		out = append(out, fmt.Sprintf("export %s=%q", key, value))
	}
	return paths.WriteFileAtomic(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

// maskSecret shows enough of a key to recognise it and nothing more.
func maskSecret(v string) string {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return "-"
	case len(v) <= 8:
		return strings.Repeat("*", len(v))
	default:
		return v[:4] + strings.Repeat("*", 6) + v[len(v)-4:]
	}
}

// readSecret obtains the key from --key, from stdin (--stdin or a piped
// stdin), or from a no-echo terminal prompt.
func readSecret(cmd *cobra.Command, flagValue string, fromStdin bool) (string, error) {
	if flagValue != "" {
		return strings.TrimSpace(flagValue), nil
	}
	in := cmd.InOrStdin()
	f, isFile := in.(*os.File)
	interactive := isFile && isatty.IsTerminal(f.Fd())
	if fromStdin || !interactive {
		data, err := io.ReadAll(bufio.NewReader(in))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	fmt.Fprint(cmd.ErrOrStderr(), "API key (input hidden): ")
	b, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "store, remove and inspect provider API keys (~/.uta/credentials.env)",
		Long: `Manage API keys for cloud providers and services uta calls directly
(Nebius Token Factory, Tavily, ...). Keys live in ~/.uta/credentials.env
with 0600 permissions, outside every repository, and are loaded into the
environment on startup when the variable is not already set. A variable
already exported in your shell always wins over the file.

  uta auth set nebius                 # prompts without echo
  uta auth set nebius --key v1.nebius...
  pbpaste | uta auth set tavily --stdin
  uta auth status
  uta auth unset nebius

Pair with 'uta limits' to cap what a key may spend per day or month.`,
	}

	var keyFlag string
	var fromStdin bool
	setCmd := &cobra.Command{
		Use:   "set <provider|VAR_NAME>",
		Short: "store an API key (prompted without echo unless --key or --stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := credentialVarFor(args[0])
			if err != nil {
				return err
			}
			secret, err := readSecret(cmd, keyFlag, fromStdin)
			if err != nil {
				return err
			}
			if secret == "" {
				return fmt.Errorf("empty key")
			}
			home, err := paths.Home()
			if err != nil {
				return err
			}
			path := paths.CredentialsFile(home)
			if err := writeCredential(path, key, secret, false); err != nil {
				return err
			}
			_ = os.Setenv(key, secret)
			fmt.Fprintf(cmd.OutOrStdout(), "stored %s (%s) in %s\n", key, maskSecret(secret), path)
			return nil
		},
	}
	setCmd.Flags().StringVar(&keyFlag, "key", "", "the key value (prefer the prompt or --stdin so it stays out of shell history)")
	setCmd.Flags().BoolVar(&fromStdin, "stdin", false, "read the key from stdin")

	unsetCmd := &cobra.Command{
		Use:   "unset <provider|VAR_NAME>",
		Short: "remove a stored API key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := credentialVarFor(args[0])
			if err != nil {
				return err
			}
			home, err := paths.Home()
			if err != nil {
				return err
			}
			path := paths.CredentialsFile(home)
			if err := writeCredential(path, key, "", true); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s from %s\n", key, path)
			return nil
		},
	}

	var statusJSON bool
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "show which keys are configured, masked, and where they come from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := paths.Home()
			if err != nil {
				return err
			}
			path := paths.CredentialsFile(home)
			lines, _ := readCredentialsFile(path)
			inFile := map[string]bool{}
			for _, l := range lines {
				if k := credentialLineKey(l); k != "" {
					inFile[k] = true
				}
			}
			seen := map[string]bool{}
			var vars []string
			for _, v := range credentialVars {
				if !seen[v] {
					seen[v] = true
					vars = append(vars, v)
				}
			}
			for k := range inFile {
				if !seen[k] {
					seen[k] = true
					vars = append(vars, k)
				}
			}
			sort.Strings(vars)
			type row struct {
				Var    string `json:"var"`
				Set    bool   `json:"set"`
				Masked string `json:"masked"`
				Source string `json:"source"`
			}
			var rows []row
			for _, v := range vars {
				val, ok := os.LookupEnv(v)
				src := "-"
				switch {
				case ok && inFile[v]:
					src = "credentials.env"
				case ok:
					src = "environment"
				case inFile[v]:
					src = "credentials.env (not loaded)"
				}
				rows = append(rows, row{Var: v, Set: ok && val != "", Masked: maskSecret(val), Source: src})
			}
			if statusJSON {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"file": path, "keys": rows})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "credentials file: %s\n\n", path)
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "VARIABLE\tSET\tVALUE\tSOURCE")
			for _, r := range rows {
				set := "no"
				if r.Set {
					set = "yes"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Var, set, r.Masked, r.Source)
			}
			return tw.Flush()
		},
	}
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "emit as JSON")

	cmd.AddCommand(setCmd, unsetCmd, statusCmd)
	return cmd
}
