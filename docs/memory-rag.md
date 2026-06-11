# Institutional memory & hybrid recall

uta remembers. Audit findings, session outcomes, and consolidated facts
land in the `institutional_facts` table — and, since this version, in a
hybrid retrieval index inside the same SQLite database:

- **FTS5 / BM25** keyword index — always on, zero dependencies.
- **sqlite-vec KNN** vector index — on when an embedder is configured.
  Pure Go (`modernc.org/sqlite/vec`), no CGO, no separate process.

Both lists are fused with reciprocal rank fusion (RRF, k=60).

## Searching

```sh
uta recall "reentrancy fixes we applied before"
uta recall "gemini workspace trust" --k 10 --format json
```

The planner uses the same retrieval automatically: when a profile opts
into memory injection, the "## Relevant prior facts" block now comes
from hybrid search over the goal text (falling back to v1 tag-overlap
on empty results).

## Embedder configuration (optional)

| Env | Effect |
|---|---|
| _unset_, no `GEMINI_API_KEY` | keyword-only retrieval |
| `GEMINI_API_KEY=...` | Gemini embedding API (`gemini-embedding-001`) |
| `UTA_EMBEDDER=ollama` | local Ollama (`nomic-embed-text`) |
| `UTA_EMBEDDER=off` | force keyword-only |
| `UTA_EMBED_MODEL=...` | override the model for either backend |

Embedding failures degrade gracefully — documents are still keyword-
indexed, queries still return BM25 results.

## Reindexing

```sh
uta recall --reindex
```

Run once after upgrading (facts written before this version aren't in
the index yet), or after switching embedders — the index refuses to mix
vector spaces from different models and will tell you when a rebuild is
needed.
