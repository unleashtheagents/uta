#!/usr/bin/env bash
# scripts/self-improve.sh — paste-safe wrapper around `uta improve`.
#
# Usage:
#   ./scripts/self-improve.sh                       # use defaults
#   BUDGET=2h MAX_ITER=30 ./scripts/self-improve.sh # override caps
#   VERIFY='go test -race ./...' ./scripts/self-improve.sh
#   GOAL='test coverage gaps' ./scripts/self-improve.sh
#
# Defaults are tuned for Go projects but every knob is generic — VERIFY is
# the only language-specific value, and you can override it for Node /
# Python / Rust / Solidity by exporting a different command.
set -euo pipefail

cd "$(dirname "$0")/.."   # repo root

: "${WORKER:=claude}"
: "${GATHER_WORKER:=gemini}"
: "${BUDGET:=4h}"
: "${MAX_ITER:=50}"
: "${PER_IDEA:=20m}"
: "${VERIFY:=go test ./...}"
: "${GOAL:=robustness, tests, and code quality}"
: "${PRE_APPROVE:=Read,Edit,Write,Bash(go *),Bash(gofmt *),Bash(golangci-lint *)}"

# Show resolved config so a paste-mangled override can't hide.
cat <<EOM
self-improve config:
  WORKER         $WORKER
  GATHER_WORKER  $GATHER_WORKER
  VERIFY         $VERIFY
  GOAL           $GOAL
  BUDGET         $BUDGET
  MAX_ITER       $MAX_ITER
  PER_IDEA       $PER_IDEA
  PRE_APPROVE    $PRE_APPROVE
EOM

# Sanity: working tree must be clean. If not, refuse to run rather than
# tangle the agent's edits with whatever is already in flight.
if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "abort: working tree has uncommitted changes. Commit or stash first." >&2
  git status -s >&2
  exit 1
fi

# Ensure we're on a feature branch so review-and-merge is clean.
branch=$(git symbolic-ref --short HEAD)
if [ "$branch" = "main" ] || [ "$branch" = "master" ]; then
  echo "abort: refuse to run on '$branch'. Create a branch first:" >&2
  echo "  git switch -c improve-\$(date +%Y%m%d-%H%M)" >&2
  exit 1
fi

exec uta improve \
  --verify "$VERIFY" \
  --worker "$WORKER" \
  --gather-worker "$GATHER_WORKER" \
  --budget "$BUDGET" \
  --max-iter "$MAX_ITER" \
  --per-idea-timeout "$PER_IDEA" \
  --gather-when-empty \
  --gather-goal "$GOAL" \
  --pre-approve "$PRE_APPROVE"
