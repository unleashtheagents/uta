package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/otel"
	"github.com/unleashtheagents/uta/internal/store"
)

func newTrajectoryCmd() *cobra.Command {
	var (
		format       string
		limit        int
		offset       int
		otlpEndpoint string
	)
	cmd := &cobra.Command{
		Use:   "trajectory <session-id>",
		Short: "render the event timeline of a run",
		Long: `Render the recorded trajectory of one session.

Formats:
  pretty   human-readable timeline (default)
  jsonl    one event per line
  json     full event array
  otlp     OpenTelemetry trace (OTLP/JSON, GenAI semantic conventions) —
           pipe to a file or POST straight to a collector with
           --otlp-endpoint http://localhost:4318

The OTLP trace has deterministic ids (derived from session/subtask ids)
so re-exporting the same session updates rather than duplicates it in
the collector.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			id, err := app.Store.ResolveSessionID(args[0])
			if err != nil {
				return err
			}
			sess, err := app.Store.GetSession(id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("session not found: %s", id)
				}
				return err
			}
			events, err := app.Store.ListEvents(id, limit, offset)
			if err != nil {
				return err
			}

			if otlpEndpoint != "" && format != "otlp" {
				return errors.New("--otlp-endpoint requires --format otlp")
			}

			switch format {
			case "jsonl":
				enc := json.NewEncoder(cmd.OutOrStdout())
				for _, ev := range events {
					if err := enc.Encode(ev); err != nil {
						return err
					}
				}
				return nil
			case "json":
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(events)
			case "otlp":
				subtasks, err := app.Store.SubtaskListBySession(id, 0, 0)
				if err != nil {
					return err
				}
				trace := otel.BuildTrace(sess, subtasks, events)
				if otlpEndpoint != "" {
					return pushOTLP(cmd, otlpEndpoint, trace)
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(trace)
			case "pretty", "":
				return prettyPrintTrajectory(cmd.OutOrStdout(), sess, events)
			default:
				return fmt.Errorf("unknown format: %s", format)
			}
		},
	}
	cmd.Flags().StringVar(&format, "format", "pretty", "output format: pretty|jsonl|json|otlp")
	cmd.Flags().IntVar(&limit, "limit", 0, "max events to return (0 for all)")
	cmd.Flags().IntVar(&offset, "offset", 0, "events to skip before returning results")
	cmd.Flags().StringVar(&otlpEndpoint, "otlp-endpoint", "", "POST the OTLP trace to this collector base URL (e.g. http://localhost:4318); requires --format otlp")
	return cmd
}

// pushOTLP POSTs the trace to <endpoint>/v1/traces per the OTLP/HTTP
// spec. Accepts a base URL or a full /v1/traces URL — both work, so
// copy-pasted collector addresses don't trip people up.
func pushOTLP(cmd *cobra.Command, endpoint string, trace otel.ExportTraceServiceRequest) error {
	url := strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(url, "/v1/traces") {
		url += "/v1/traces"
	}
	body, err := json.Marshal(trace)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("push OTLP: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("push OTLP: collector returned HTTP %d: %s", resp.StatusCode, truncateLine(buf.String(), 200))
	}
	spanCount := 0
	for _, rs := range trace.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			spanCount += len(ss.Spans)
		}
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "pushed %d span(s) to %s\n", spanCount, url)
	return nil
}

func prettyPrintTrajectory(w interface{ Write([]byte) (int, error) }, sess store.Session, events []store.EventRow) error {
	header := fmt.Sprintf("session %s  worker=%s  status=%s  goal=%s\n\n",
		shortID(sess.ID), sess.Worker, sess.Status, truncateLine(sess.Goal, 80))
	if _, err := w.Write([]byte(header)); err != nil {
		return err
	}
	for _, ev := range events {
		line := fmt.Sprintf("%s  %-22s  %s  %s\n",
			ev.Ts.Format(time.TimeOnly),
			ev.Kind,
			shortID(ifEmpty(ev.SubtaskID, "-")),
			summarizePayload(ev.Payload),
		)
		if _, err := w.Write([]byte(line)); err != nil {
			return err
		}
	}
	return nil
}

func summarizePayload(p json.RawMessage) string {
	if len(p) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(p, &m); err != nil {
		s := string(p)
		return truncateLine(s, 100)
	}
	// Prefer text-bearing keys.
	for _, k := range []string{"title", "goal", "text", "error", "reason", "status", "spec_id"} {
		if v, ok := m[k].(string); ok && v != "" {
			return truncateLine(k+"="+v, 100)
		}
	}
	for _, k := range []string{"chars", "subtasks", "max_parallel"} {
		if v, ok := m[k]; ok {
			return truncateLine(fmt.Sprintf("%s=%v", k, v), 100)
		}
	}
	return truncateLine(string(p), 100)
}

func ifEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
