---
name: nemotron-tier-router
description: Route multi-agent tasks across NVIDIA Nemotron 3 Ultra, Super, Nano, Lightning, and Super-VL on Nebius Token Factory.
---

# Nemotron Tier Router Skill

Use inline `[model:<tier>]` directives (`[model:ultra]`, `[model:super]`, `[model:nano]`, `[model:lightning]`, `[model:vl]`) in `uta.yaml` subtasks or `steer` prompts to dispatch each step of a workflow to its optimal NVIDIA Nemotron model on Nebius Token Factory (`https://api.tokenfactory.nebius.com/v1`).
