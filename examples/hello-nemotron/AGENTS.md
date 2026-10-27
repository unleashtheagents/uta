# Hello Nemotron Workspace Rules (AGENTS.md)

- Always route architectural planning and final synthesis (10% of tokens) to **NVIDIA Nemotron 3 Ultra** (`nvidia/Nemotron-3-Ultra-550b-a55b` via `[model:ultra]`).
- Route parallel coding subtasks to **NVIDIA Nemotron 3 Super** (`nvidia/nemotron-3-super-120b-a12b` via `[model:super]`).
- Route rapid context summarization to **NVIDIA Nemotron 3 Nano** (`nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B` via `[model:nano]`).
- Screen every workspace-changing tool call with a ~300 ms OWASP Agentic ASI01–ASI10 PASS/FAIL verdict with **NVIDIA Nemotron 3.5 Lightning** (`nvidia/Nemotron-3_5-Lightning` via `[model:lightning]`).
