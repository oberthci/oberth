---
name: oberth-agent-loop
description: Drive Oberth CI from an agent session - push, wait for runs and events without polling, service assisted-continuation holds before they are abandoned, and decide what counts as evidence. Use when working with the oberth MCP tools (run_wait, events_wait, run_hold_*, promote) or the oberth CLI.
---

# Oberth agent loop

Oberth runs CI for every push to its git remote. The `oberth` MCP server
(this plugin) and the `oberth` CLI reach the same server. Record fields are
snake_case.

## Waiting: block on the server, never poll

- One run: `run_wait {"id": "<run-id>", "until": "finished"}` blocks up to
  `timeout_s` (default and maximum 600) and returns `state` (`finished`,
  `held`, `changed`, `timeout`), the run summary, its holds and `retry_hint`.
  On `timeout`, call it again.
- Several runs, holds, promotions: `events_wait {"after_id": <cursor>, "own": true}`
  blocks until any matching event after the cursor (filters: `repos`,
  `refs_glob`, `kinds`, `run_ids`, `own`), or `timed_out`. Always pass the
  returned `next_after_id` as the next `after_id`; without `after_id` it waits
  for the next event. One call covers every run and hold of a worker.
- After a gap or a restart, read what happened in order with `events_since`.
- Pushed events (notifications on the MCP session) arrive only in some
  interactive sessions; do not wait for them. `run_wait` and `events_wait`
  work everywhere.
- CLI equivalents: `oberth run wait <id>`, `oberth events --since <n> --wait`,
  `oberth sync status` for native Argo locks.
- Lists: `run_list {"own": true}` is your runs, `run_list {"held": true}`
  the running assisted runs with a parked hold.

## Failures

`retry_hint.class` says what to do: `infrastructure` means retry unchanged
(`run_retry` or `run_rerun`); `code` means read `run_logs` for the failed step
(filter with `pattern`) and fix the code. A step named `frag-<hash>-<template>`
comes from a pinned pipeline fragment of another repository.

## Assisted continuation (holds)

A push with `-o oberth.assist=1` parks a failing step's retry attempt instead
of failing the run: event `run.held` with `step`, `pod` and `abandon_at`.

- Service the hold before `abandon_at` (the server's TTL; each action moves
  it, up to twice the TTL). `run_hold_list` shows the current `abandon_at`.
  An unserviced hold is abandoned and the run fails (`run.hold_abandoned`).
- Diagnose with `run_hold_exec` (argv, no shell), change files with
  `run_hold_patch` (unified diff on `/work/src`), then `run_continue` to run
  the step's real command, or `run_hold_abandon` to let it fail.
- Any uplink whose scope covers the run's repository may service the hold.

## Evidence

- An assisted run - any exec, patch or continue - is never evidence: not for
  promotion, release or "green at this SHA", whatever its final status. Push
  the fix as a commit and wait for a real green run of that commit.
- `sync` parks a WIP branch upstream without a green gate; it is not
  completion evidence either.
- Only a passed, unassisted run of the exact SHA, or a passed promotion
  (`promote_status`), counts. `promote` takes the target branch as `branch`.

## Connection

The plugin's server URL is `OBERTH_MCP_URL` (default
`https://localhost:30443/mcp`). The token comes from `OBERTH_TOKEN_COMMAND`
or the owner-only file `~/.config/oberth/token`; that file is sent only to the
server `~/.config/oberth/env` (or `mcp.json`) names, so set `OBERTH_MCP_URL` to
that address when using it. Claude Code must trust the
server's CA (`oberth client setup` configures it, or set
`NODE_EXTRA_CA_CERTS` to the CA file).
