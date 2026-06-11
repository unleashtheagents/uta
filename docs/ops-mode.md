# ops mode — operational triage + draft publishing

`--mode ops` wires uta to the parts of the organization's stack that the
inbox/agenda agent and the communications agent need: Gmail for read-side
signals, a CMS (WordPress or Ghost) for write-side staging, and an
`ORG_STATE.md` document in the project context dir for cross-run memory.

This document is a smoke test you can run by hand. It's intentionally
not automated — the external services it touches (Google, your CMS)
require credentials we don't want in CI.

## Prerequisites

```sh
# Gmail OAuth blob (consult @modelcontextprotocol/server-gmail docs).
export UTA_OPS_GMAIL_OAUTH=$HOME/.config/uta/gmail-oauth.json

# Pick exactly one CMS.
export UTA_OPS_CMS=wordpress    # or "ghost"

# WordPress credentials (only when UTA_OPS_CMS=wordpress).
export UTA_OPS_WORDPRESS_URL=https://your-site.example.com
export UTA_OPS_WORDPRESS_USER=editor@your-site.example.com
export UTA_OPS_WORDPRESS_APP_PASSWORD=xxxx\ xxxx\ xxxx\ xxxx

# Ghost credentials (only when UTA_OPS_CMS=ghost).
export UTA_OPS_GHOST_URL=https://your-site.example.com
export UTA_OPS_GHOST_ADMIN_KEY=<admin-api-key>
```

Drop the personas + profile into place:

```sh
mkdir -p ~/.uta/personas ~/.uta/profiles
cp examples/personas/comms-agent.yaml ~/.uta/personas/
cp examples/profiles/ops.yaml ~/.uta/profiles/
# (chief-of-staff persona ships in an earlier iter; copy similarly if not
# already installed)
```

Confirm the profile and persona are visible:

```sh
uta mode show ops
uta mode list
```

## Smoke run

From inside a uta project (`uta project init` if you haven't yet):

```sh
uta run \
  -g "Read this week's investor emails. Update ORG_STATE.md with new commitments. Draft a news post about Q3 goals." \
  --mode ops
```

Expected outcomes:

1. The Gmail probe succeeds and the gmail tools are pre-approved (you'll
   see `mcp__gmail__*` in the trajectory's `pre-approved tools` event).
2. The CMS you chose (matching `UTA_OPS_CMS`) probes successfully; the
   *other* CMS probe fails with a clean error and is logged to stderr —
   the run continues.
3. `.uta/context/ORG_STATE.md` is created or updated with:
   - `last_updated` stamped to the run's UTC clock.
   - `source_emails` listing the message ids the agent read.
   - `pending_decisions` extended with any open items the emails surfaced.
   - A markdown body containing a `## Commitments` section.
4. A draft post exists in the CMS (visible in WP-Admin → Posts → Drafts,
   or Ghost-Admin → Posts → Drafts). Its status MUST be `draft` or
   `pending`. Reject the run if it is `publish` or `future`.
5. The process exits 0. When iter 16 lands the HITL gate (severity
   `medium` is already configured in the ops profile), the run will pause
   on the publish-shaped action instead of completing.

## What "draft, never publish" means here

The comms-agent persona refuses to publish without explicit operator
approval. If you see a published post or a sent email after a run, that
is a bug — open an issue and attach the trajectory id from
`uta sessions list --latest`.

## ORG_STATE.md format reference

Backed by `internal/orgstate`. The file lives at
`<project>/.uta/context/ORG_STATE.md` and looks like:

```markdown
---
last_updated: 2026-05-18T12:00:00Z
source_emails:
  - "thread-019x4..."
  - "thread-019y8..."
pending_decisions:
  - "Approve hire for staff engineer slot"
  - "Sign Acme MSA before EOQ"
---
## Commitments

- Ship v1 by end of Q3.
- Land three design-partner customers by 2026-06-30.

## 2026-05-18 — investor sync follow-ups

Notes from the Tuesday call...
```

A missing frontmatter fence is tolerated on first write (the whole file
becomes the body), but `Write` always emits the canonical layout above.

## Troubleshooting

| symptom | likely cause |
| --- | --- |
| `mcp[wordpress]: launch: ...` on every run | wrong `UTA_OPS_WORDPRESS_*` values; check `npx -y @automattic/mcp-wordpress-remote` runs by hand |
| `mcp[ghost]: launch: ...` on every run | wrong `UTA_OPS_GHOST_*` values; check the admin API key has not been rotated |
| Run completes but no draft appears | persona may have refused — check trajectory for the refusal text |
| `orgstate: parse frontmatter` error | someone hand-edited ORG_STATE.md and broke the YAML — fix the fence and re-run |
