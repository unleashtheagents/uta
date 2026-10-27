---
name: nebius-nemotron-coordinator
description: Coordinate NVIDIA Nemotron 3 Ultra, Super, Lightning, and Super-VL models on Nebius Token Factory alongside Claude Code and Antigravity (agy).
---

# Nebius Token Factory × NVIDIA Nemotron Agent Coordinator

Use this skill when orchestrating multi-agent workflows with `uta` on **Nebius Token Factory** (`https://api.tokenfactory.nebius.com/v1`).

## 1. Model Tiers & Aliases (`uta nebius models`)

| Alias | Nebius Token Factory Model ID | Architecture & Params | Role |
|---|---|---|---|
| `ultra` | `nvidia/Nemotron-3-Ultra-550b-a55b` | Hybrid Mamba-2 + Transformer MoE (550B / 55B active, 1M ctx) | 10% Planner, DAG Decomposer & Adversarial Synthesizer |
| `super` | `nvidia/nemotron-3-super-120b-a12b` | Hybrid Mamba-2 + Transformer MoE (120B / 12B active, 1M ctx) | 90% High-Throughput Parallel Worker Swarm (148 tok/s) |
| `lightning` | `nvidia/Nemotron-3_5-Lightning` | Latent Mamba-Attention (30B / 3.5B active, 256K ctx) | ~300 ms OWASP ASI01-ASI10 Sentinel & Budget Guardrail |
| `vl` | `nvidia/Nemotron-3.5-Super-VL` | Multimodal Vision-Language MoE (120B / 12B active + ViT) | UI Regression & Architecture Diagram Analysis |

## 2. Recommended Commands

```sh
# Inspect Nebius Token Factory status, model tiers, and discovered skills
uta nebius status
uta nebius models

# Classify a goal into the optimal 90/10 Nemotron tier
uta nebius route "design a zero-trust MCP gateway"

# Run with automatic workspace rules (AGENTS.md / CLAUDE.md / GEMINI.md) and @file expansion
UTA_NEBIUS_INJECT_RULES=1 NEBIUS_MODEL=ultra uta run -g "audit @internal/provider/builtin/nebius.go" --worker nebius -y

# Run deterministic Steer mission
uta mission run examples/nebius-nemotron.steer
```
