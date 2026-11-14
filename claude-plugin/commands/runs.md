---
description: Show recent Oberth runs as a table (read-only; requires function hooks)
argument-hint: "--limit=N"
---

The Oberth function hooks module handles /oberth:runs locally. If this
message reaches the model, function hooks are unavailable in this client.
Tell the user: "The Oberth live display needs a client with function hooks.
Existing Oberth commands still work; use the run_list and run_get MCP tools
for read-only run details."
Do not start, cancel, rerun or approve a run.
