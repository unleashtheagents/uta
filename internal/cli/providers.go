package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/provider"
)

func newProvidersCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "list detected agent providers",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			detections := app.Registry.DetectAll(cmd.Context())

			if asJSON {
				type row struct {
					Name         string                `json:"name"`
					Available    bool                  `json:"available"`
					BinaryPath   string                `json:"binary_path,omitempty"`
					Version      string                `json:"version,omitempty"`
					Capabilities []provider.Capability `json:"capabilities,omitempty"`
					Notes        string                `json:"notes,omitempty"`
					Error        string                `json:"error,omitempty"`
				}
				out := make([]row, 0, len(detections))
				for _, name := range app.Registry.Names() {
					d := detections[name]
					errStr := ""
					if d.Err != nil {
						errStr = d.Err.Error()
					}
					out = append(out, row{
						Name:         name,
						Available:    d.Available,
						BinaryPath:   d.BinaryPath,
						Version:      d.Version,
						Capabilities: d.Capabilities,
						Notes:        d.Notes,
						Error:        errStr,
					})
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATUS\tVERSION\tBINARY\tCAPABILITIES\tNOTES")
			for _, name := range app.Registry.Names() {
				d := detections[name]
				status := "missing"
				if d.Available {
					status = "ok"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					name, status, dashIfEmpty(d.Version), dashIfEmpty(d.BinaryPath),
					capsString(d.Capabilities), dashIfEmpty(d.Notes))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of a table")
	return cmd
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
