---
name: oberth-agent-loop
description: Drive Oberth CI from an agent session - push, wait for runs and events without polling, service assisted-continuation holds before they are abandoned, and decide what counts as evidence. Use when working with the oberth MCP tools (run_wait, events_wait, run_hold_*, promote) or the oberth CLI.
---

# Oberth agent loop

Oberth runs CI for every push to its git remote. The `oberth` MCP server
(this plugin) and the `oberth` CLI reach the same server. Record fields are
snake_case.

## Waiting: keep one events_wait open, never poll

While this session has runs in flight (a push, a retry, a promotion), keep
exactly one call open:

```json
events_wait {"kinds": ["run.held", "run.finished"], "own": true, "after_id": <cursor>, "timeout_s": 600}
```

- Start it right after the push, in the background when the client offers
  that for long tool calls, and go on working: its result wakes the session.
  `own` limits it to runs this uplink pushed; add `promotion.finished` to
  `kinds` while a promotion runs.
- On a result, act on every event (`run.held`: service the hold before
  `abandon_at`; `run.finished`: read `status` and `retry_hint`), then call it
  again with the returned `next_after_id` as `after_id`. On `timed_out` do the
  same. Stop when nothing is in flight.
- The first call may omit `after_id`: it then waits for the next event and
  does not return one recorded before the call. So start the first wait
  before the push, or check the run once after starting it
  (`run_get {"id": "<run-id>"}`).
- Events carry an increasing `id`. Remember the highest one handled and skip
  anything at or below it: a pushed copy of the same event (some interactive
  sessions receive `notifications/claude/channel`) is then harmless. Never
  wait for pushed events; they are an extra, not the protocol.
- One run only: `run_wait {"id": "<run-id>", "until": "finished"}` blocks up to
  `timeout_s` (default and maximum 600) and returns `state` (`finished`,
  `held`, `changed`, `timeout`), the run summary, its holds and `retry_hint`.
- After a restart or a gap, read what happened in order with
  `events_since {"after_id": <cursor>}`.
- Other `events_wait` filters: `repos`, `refs_glob`, `run_ids`.
- CLI equivalents: `oberth run wait <id>`, `oberth events --since <n> --wait`,
  `oberth sync status` for native Argo locks.
- Lists: `run_list {"own": true}` is your runs, `run_list {"held": true}`
  the running assisted runs with a parked hold.

## Failures

`retry_hint.class` says what to do: `infrastructure` means retry unchanged
(`run_rerun`); `code` means list native containers with
`run_logs {"id":"RUN","containers":true}`, then read the failed node's
`node_id` and `container` with a `pattern` filter and fix the code. The `logs`
tool accepts the same native selectors with `repo` and `sha`.

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
  Follow the promotion ID with `promote_status {"id":"<promotion-id>"}`.
  After publication, the server reads the forge target with its configured
  deploy key; a passed result records `promotion.observed_target_sha`, equal
  to `promotion.ResultSHA`. Use that observation to confirm publication when
  the private forge is inaccessible locally. It records the completion-time
  head; the branch can move afterward.

## Connection

The plugin asks for the server's MCP URL, the uplink token (kept in Claude
Code's credential store) and the CA SHA-256 pin; change them with
`/plugin configure oberth`. Claude Code must trust the server's CA:
`/oberth:connect` checks it against the pin and stores it, then one restart
is needed. `oberth client setup` does both from a machine that has the
Oberth CLI.

## Installer failure guardrail

When the installer fails or gives no clear reason, stay on the installer path:
read the public result JSON and its checks, endpoint, observed error and remedies.
Run only the documented installer verify or resume command as printed, preserving
the reviewed context and answers. If the result has no actionable reason, stop
and ask the user; never invent a workaround outside the documented commands.
Never run administrative commands inside the server Pod (including upstream add
or enrollment), directly or through another helper, to work around an installer
failure. Do not switch to another skill to bypass this rule.
Never read, pipe or export the secret store's root token or any value from the
secrets file. Give only file paths to the documented installer or unseal command;
the executable reads credentials privately. Never ask the user to paste a token
or export BAO_TOKEN for the agent.
Report only verified facts. An installer exit code or an incomplete result does
not establish whether anything was pushed to the forge. Say what was observed,
what remains deployed and what is unknown; never claim that nothing was pushed
without checking evidence that establishes that fact.

## Report the connected tool count

<!-- BEGIN GENERATED MCP TOOL COUNT -->
The full server registry contains **92 tools**, including administrator-only
operations. The authenticated `tools/list` response is authoritative for the
connected identity; non-admin clients do not receive administrator-only tools.
<!-- END GENERATED MCP TOOL COUNT -->

When asked how many tools are available, report the number the client currently
shows for the Oberth server. If the client offers a tool inventory, count that
inventory. Say when the client shows only a combined count for several servers;
do not attribute that total to Oberth. If the visible inventory is unavailable,
say the connected count is unknown. Do not substitute this checkout's registry
count: permissions, client filtering and server versions can change the visible
inventory.
