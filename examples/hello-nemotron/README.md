# Hello Nemotron — Multi-Model Nebius Token Factory Sample Project

This sample project demonstrates how `uta` coordinates **four distinct NVIDIA Nemotron models** on **Nebius Token Factory (`https://api.tokenfactory.nebius.com/v1`)** inside a single workflow (`uta.yaml`) or a single deterministic `steer` program (`hello-nemotron.steer`):

| Directive | Model ID on Nebius Token Factory | Active Params | Role in Hello World |
|---|---|---|---|
| `[model:ultra]` | `nvidia/Nemotron-3-Ultra-550b-a55b` | 550B / 55B MoE | **10% Architect & Planner** (`s1_ultra_architect` / `ultra_plan`) |
| `[model:super]` | `nvidia/nemotron-3-super-120b-a12b` | 120B / 12B MoE | **90% Parallel Worker Swarm** (`s2_super_worker` / `super_implement`) |
| `[model:nano]` | `nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B` | 30B / 3B MoE | **Fast Context Summarizer** (`s3_nano_summarizer` / `nano_compact`) |
| `[model:lightning]` | `nvidia/Nemotron-3_5-Lightning` | 30B / 3.5B | **Sub-50ms OWASP ASI Sentinel** (`s4_lightning_sentinel` / `lightning_guardrail`) |
| `[model:vl]` | `nvidia/Nemotron-3.5-Super-VL` | 120B / 12B + ViT | **Multimodal UI & Diagram Analyst** |

## Quickstart Commands

```sh
# 1. Inspect discovered AGENTS.md rules and SKILL.md skills in this sample project
cd examples/hello-nemotron
uta skills list

# 2. Run the 4-model parallel workflow (Ultra + Super + Nano + Lightning concurrently!)
cd ../..
uta run -f examples/hello-nemotron/uta.yaml -y

# 3. Check, test offline, and run the 4-model steer mission live on Nebius Token Factory
uta mission check examples/hello-nemotron/hello-nemotron.steer
uta mission test examples/hello-nemotron/hello-nemotron.steer --script examples/hello-nemotron/hello-nemotron.journal.yaml
uta mission run examples/hello-nemotron/hello-nemotron.steer --worker nebius
```
