#!/usr/bin/env bash
# Interactive Demo Video Driver for uta-devpost-hackathon
# Run from ~/projects/uta: ./examples/hello-nemotron/demo-recording.sh

set -e

pause_step() {
  echo ""
  printf "\033[1;36m━━━ %s ━━━\033[0m\n" "$1"
  if [ "${AUTO_DEMO:-0}" != "1" ]; then
    read -r -p "Press [Enter] to run..." _
  fi
}

run_cmd() {
  printf "\033[1;32m$ %s\033[0m\n" "$*"
  eval "$*"
}

pause_step "SCENE 1: Nebius Token Factory × NVIDIA Nemotron Status & Live 88ms Ping"
run_cmd "uta nebius status"
echo ""
run_cmd "uta nebius ping"

pause_step "SCENE 2: Unified agy/claude Skills + 90/10 Router + 4-Model Parallel Hello World"
run_cmd "uta skills list"
echo ""
run_cmd 'uta nebius route "design a zero-trust MCP gateway and add unit tests"'
echo ""
run_cmd "uta run -f examples/hello-nemotron/uta.yaml -y"

pause_step "SCENE 3: Live Tavily Search + Nemotron Synthesis & Multi-Hop Tool Trajectory"
run_cmd 'uta nebius search "NVIDIA Nemotron 3 hybrid Mamba-2 Transformer MoE architecture" --max-results 3'
echo ""
run_cmd "uta trajectory 29643605"

pause_step "SCENE 4: steer Language — Static Check, Golden Journal Test, and Live 4-Model Mission"
run_cmd "uta mission check examples/hello-nemotron/hello-nemotron.steer"
run_cmd "uta mission test examples/hello-nemotron/hello-nemotron.steer --script examples/hello-nemotron/hello-nemotron.journal.yaml"
run_cmd "uta mission run examples/hello-nemotron/hello-nemotron.steer --worker nebius"

pause_step "SCENE 5: Live Nebius vs Local CLI Benchmark Summary"
run_cmd "uta nebius models"
