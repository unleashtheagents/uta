package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/memory"
)

func newRecallCmd() *cobra.Command {
	var (
		k       int
		format  string
		reindex bool
	)
	cmd := &cobra.Command{
		Use:   "recall [query]",
		Short: "search institutional memory (hybrid BM25 + vector retrieval)",
		Long: `Search everything uta has remembered — audit findings, session outcomes,
consolidated facts — with hybrid retrieval: FTS5/BM25 keyword ranking
fused (reciprocal rank fusion) with vector KNN when an embedder is
configured.

Embedder configuration (optional — keyword search needs nothing):

  UTA_EMBEDDER=gemini   use the Gemini embedding API (needs GEMINI_API_KEY)
  UTA_EMBEDDER=ollama   use a local Ollama server (default model
                        nomic-embed-text; override via UTA_EMBED_MODEL)
  unset                 auto: gemini when GEMINI_API_KEY is set, else
                        keyword-only

--reindex rebuilds the retrieval index from the institutional_facts
table. Run it once after upgrading to this version (facts written
before the index existed aren't searchable until then) or after
switching embedders.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			mem := memory.NewFromEnv(app.Store.DB)

			if reindex {
				start := time.Now()
				indexed, embedded, err := mem.Reindex(cmd.Context())
				if err != nil {
					return fmt.Errorf("reindex: %w", err)
				}
				embedderLabel := "(none — keyword index only)"
				if mem.Embedder != nil {
					embedderLabel = mem.Embedder.Name()
				}
				fmt.Fprintf(cmd.ErrOrStderr(),
					"reindexed %d documents (%d embedded) in %s · embedder=%s\n",
					indexed, embedded, time.Since(start).Round(time.Millisecond), embedderLabel)
				if len(args) == 0 {
					return nil
				}
			}

			if len(args) == 0 {
				return errors.New("pass a query, or --reindex to rebuild the index")
			}
			query := strings.TrimSpace(args[0])
			results, err := mem.SearchHybrid(cmd.Context(), query, k)
			if err != nil {
				return err
			}

			switch format {
			case "json":
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(results)
			case "", "pretty":
				renderRecallResults(cmd, results, query, mem.Embedder != nil)
				return nil
			default:
				return fmt.Errorf("unknown format: %s", format)
			}
		},
	}
	cmd.Flags().IntVarP(&k, "k", "k", 5, "max results")
	cmd.Flags().StringVar(&format, "format", "pretty", "output format: pretty|json")
	cmd.Flags().BoolVar(&reindex, "reindex", false, "rebuild the retrieval index from institutional facts first")
	return cmd
}

func renderRecallResults(cmd *cobra.Command, results []memory.SearchResult, query string, hasEmbedder bool) {
	out := cmd.OutOrStdout()
	retrievalMode := "keyword (BM25)"
	if hasEmbedder {
		retrievalMode = "hybrid (BM25 + vector)"
	}
	fmt.Fprintf(out, "recall %q · %s · %d result(s)\n\n", query, retrievalMode, len(results))
	if len(results) == 0 {
		fmt.Fprintln(out, "(nothing relevant — has anything been remembered yet? see `uta memory stats`,")
		fmt.Fprintln(out, " and run `uta recall --reindex` once after upgrading)")
		return
	}
	for i, r := range results {
		mode := r.ModeName
		if mode == "" {
			mode = "no-mode"
		}
		fmt.Fprintf(out, "%2d. [%s · %s] score=%.4f via %s\n",
			i+1, mode, r.Kind, r.Score, strings.Join(r.Matched, "+"))
		body := r.Body
		const maxBody = 400
		if len(body) > maxBody {
			body = body[:maxBody] + "…"
		}
		for _, line := range strings.Split(body, "\n") {
			fmt.Fprintf(out, "    %s\n", line)
		}
		fmt.Fprintf(out, "    — %s · %s\n\n", r.Ref, r.CreatedAt.Format("2006-01-02"))
	}
}
