# Agent events and the MCP agent loop

An agent connected only to Oberth's MCP endpoint can drive a branch from push
to land without background scripts (#1514). Oberth records what an agent
would otherwise poll for as durable **agent events**, pushes them to connected
MCP sessions, lets a tool call block until a run finishes or holds, reports
native Argo lock holders and their history, and prunes a promoted branch only
after it verified the promotion upstream. Everything is generic Oberth and
Argo behaviour, scoped by the caller's uplink token. No run status, label or
layout is added.

## Event kinds

| Kind | When | Data |
|------|------|------|
| `run.finished` | a run reaches passed, failed or interrupted | `status`, `display_status`, `display_reason`, `phase`, `failed_burn`, `failed_step`, bounded `error`, `retry_hint` (`class`, `cause`, `action`), `actor`, `trigger`, `release`, `assisted` |
| `run.held` | an assisted retry hold parks (once per held Pod) | `step`, `pod`, `attempt`, `held_since`, `abandon_at` (= `held_since` + `assist.holdTTL`: the idle deadline when the event is recorded; it is not updated, while the server-side abandon deadline moves with each exec, patch or continue, up to `held_since` + 2 × `assist.holdTTL`), `hold_ttl_s`, `holds_in_run`, `multiple_holds`, `held_steps`, `patch_capable` |
| `run.hold_abandoned` | a held Pod was abandoned: by the server sweep past `assist.holdTTL` or above `assist.maxConcurrentHolds`, or by the agent (once per Pod) | `step`, `pod`, `abandon_reason` (`ttl`, `cap` or `agent`), `abandoned_by`, `held_since`, `abandoned_at`, `failed_attempt`, `failed_exit_code` |
| `run.lock_wait` | a running run waits on one native Argo semaphore or mutex longer than `agentEvents.lockWaitThreshold` (once per wait); since v0.20.0 also a Workflow the Argo controller postponed for its parallelism | `lock`, `waiting_since`, `waited_s`, `holders` (run IDs), `holder_count`; for an uplink-scoped reader `holders` lists only runs in its scope and `hidden_holders` counts the others (`holder_count` stays the total); for parallelism `lock: Parallelism/workflow-controller`, `postponed: true` and `running` (Oberth Workflows Argo admitted) instead of the holders |
| `promotion.finished` | a promotion is passed, failed or interrupted | `status`, `source_branch`, `source_sha`, `target_ref`, `result_sha`, `run_id`, bounded `error`, `prune_source` (`status`, `reason`, `batch_id`) when requested |
| `release.published` | a release run passed (its publication is delivered) | `tag`, `sha` |
| `publication.failed` | an upstream publication failed for good, or Oberth scheduled an automatic retry | `final`, `attempt`, `max_attempts`, `next_retry_at`, `publication_id`, bounded `error` |

Every event has a monotonically increasing `id`, `kind`, `at`, `repo`, `ref`,
`sha`, `run_id` or `promotion_id`, a one-line `summary` and `data`. A fact is
recorded once: a run's terminal result, one held Pod, one lock wait interval
and one publication attempt each have a deduplication key, so a restart or a
second observer never repeats it. `run.held` lists every step a run holds, so
an agent names the right `step` for `run_hold_*` when a run holds two.

Oberth records events from the existing run and promotion change signals and
from a sweep every `agentEvents.interval` (default 10 s) for what has no
signal: lock waits, parallelism postponement and publication retries.
Parked holds have their own loop every 5 s (the hold sweep's tick) beside
that sweep, so a slow pending-prune upstream read or Workflow GET in the
sweep (up to 30 s) never delays `run.held`. It starts after
owner recovery and only reads Kubernetes: one Workflow GET per running run
(at most 64 per sweep) and the existing hold Pod list for assisted runs.

## Push to a connected session

Oberth's MCP server declares two capabilities in `initialize` and returns an
`Mcp-Session-Id`:

- `experimental["claude/channel"]`: Oberth sends each event as a
  `notifications/claude/channel` notification. `content` is the summary line
  followed by the event as JSON; `meta` carries `kind`, `event_id`, `repo`,
  `run_id`, `ref`, `sha`, `at` and the scalar fields above (`step`, `pod`,
  `held_since`, `abandon_at`, `status`, `failed_step`, `lock`, `waited_s`, `retry_class`,
  ...) as strings.
- `resources: {subscribe: true}`: `run://<id>` is a resource
  (`resources/read` returns the run detail); `resources/subscribe` on it
  sends `notifications/resources/updated` for every event about that run.

Both are delivered on the session's streamable-HTTP **GET** `/mcp` stream.
The POST endpoint stays stateless; the session only binds a filter and the
subscribed run resources to the bearer credential that opened it. A session
of another credential answers 404.

Without a call to `events_subscribe`, a session receives every kind for every
repository in its uplink scope. `events_subscribe {repos?, refs_glob?,
kinds?, own?}` narrows it; `own: true` keeps only the caller's own runs and
promotions. A scoped uplink never receives an event about a repository outside
its upstreams, through any of these paths.

### Claude Code

Configure Claude Code once per machine with `oberth client setup` (see
[MCP setup](mcp-setup.md#oberth-client-setup)). It registers Oberth as a
user-scope HTTP server whose `headersHelper` reads the token at connect time,
with `"timeout": 630000`, and trusts the CA through `env.NODE_EXTRA_CA_CERTS`
in `~/.claude/settings.json`. No environment variable, token in a file or
launcher script is needed. What a session then gets depends on one flag.

**Plain `claude` (and `claude -p`, the Agent SDK): tools, `run_wait` and
`events_since`; no push.** Every Oberth tool works. Waiting is a tool call:
`run_wait {id, timeout_s: 600}` blocks server-side until the run finishes,
holds or times out. Verified with Claude Code 2.1.322 in `-p` mode: a
`run_wait {timeout_s: 600}` on a running run returned `state: timeout` after
10 m 0 s, with no client timeout. In an interactive session Claude Code
documents moving an MCP call that runs longer than 120 s into a background
task (`CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS`) and delivering its result when it
returns, so the agent keeps working and still hears the outcome; `-p` mode
waits for the call in the foreground. After a
reconnect or a long gap, `events_since {after_id}` reads what happened in
order. Pushed events do not arrive in this mode, so do not wait for them.

The 630 s timeout matters here: Claude Code's default per-request timeout for
an HTTP MCP server is 60 s, which cuts a 600 s `run_wait` at one minute. The
registered timeout covers the longest wait plus its final heartbeat. An entry
registered by hand needs the same field (`claude mcp get oberth` shows
`Timeout: 630000ms`).

**Push: interactive sessions started with the development-channels flag.**

```sh
claude --dangerously-load-development-channels server:oberth
```

Claude Code's channels are a research preview. A session receives channel
notifications only from servers named for that session: `--channels` accepts
only plugins on Anthropic's channel allowlist (or an organisation's
`allowedChannelPlugins` managed setting), and any other server, Oberth
included, needs `--dangerously-load-development-channels`, which shows a
confirmation dialog at each start. The flag is ignored in `-p` mode and in the
Agent SDK, and no settings key, environment variable or `.mcp.json` field
replaces it. With it, Claude Code 2.1.322 logs `Channel notifications
registered` for `oberth` over HTTP and holds one of the credential's GET
streams; events then arrive in the session as
`<channel source="oberth" kind="run.held" step="test" abandon_at="..." ...>`.
Confirm on your client version with `/mcp` and `claude --debug`, and keep
`run_wait` and `events_since` as the fallback either way.

Claude Code's v2 runtime does not register a channel from a server that
negotiates MCP revision 2026-07-28; Oberth negotiates 2025-06-18 and is
unaffected.

### Delivery and bounds

- **At-least-once.** Each SSE frame carries the event ID. A client that
  reconnects with `Last-Event-ID` resumes after it; a session also remembers
  its last delivered ID. After a gap, `events_since {after_id}` returns the
  events in order; `gap: true` means older events were pruned.
- **Bounded backlog.** A connecting stream replays at most
  `agentEvents.maxBacklog` events (default 500). When more are pending it
  first sends a `kind=events.backlog_truncated` channel notice naming the
  `after_id` to read with `events_since`.
- **Streams per credential.** At most `agentEvents.maxStreamsPerToken` open
  GET streams per bearer credential (default 4); one more answers 429 with
  `Retry-After`. A credential keeps at most 16 sessions; idle sessions expire
  after an hour.
- **Heartbeat and slow readers.** A keepalive comment every
  `agentEvents.heartbeat` (default 20 s). Each frame has its own write
  deadline: a reader that stops reading is disconnected, never buffered for.
- **Authorization.** The stream re-checks its bearer token every 30 s and ends
  with a `kind=events.auth_error` notice when it was revoked or changed.
- **Restart.** A draining server closes streams; clients reconnect to the new
  process with `Last-Event-ID`. An unknown session ID from before the restart
  is adopted for the same credential.

## Tools

- `run_wait {id, until: finished|held|any, timeout_s}` blocks server-side, at
  most 600 s, and returns `state` (`finished`, `held`, `changed`, `timeout`),
  the run summary, its unreleased holds and `retry_hint`. It replaces client
  loops around `run_get`. The client's request timeout must exceed the wait:
  Claude Code needs the server entry's `"timeout": 630000` (its default is
  60 s).
- `events_since {after_id, limit, repos?, refs_glob?, kinds?, own?}` pages the
  event log in ID order; continue with `next_after_id`.
- `events_wait {after_id?, timeout_s, repos?, refs_glob?, kinds?, own?, run_ids?}`
  blocks server-side, at most 600 s, until at least one event after `after_id`
  passes the uplink scope and every filter, and returns that `events_since`
  page; on timeout `timed_out` is true, `events` is empty and `next_after_id`
  has moved past every event scanned. Without `after_id` it waits for the next
  event. One loop of `events_wait` with the returned cursor covers every run
  and hold of a worker, in sessions that receive no pushed events too. CLI:
  `oberth events --since <id> --wait`, and `oberth run wait <id>` for one run.
- Every blocking tool takes its bound as `timeout_s`; no other spelling is
  accepted.
- `run_list {own: true}` keeps the caller's own runs; `run_list {held: true}`
  lists the running assisted runs with a parked hold no action released.
- `sync_status {lock?, since?, limit?}` returns, per native Argo lock key
  (`ConfigMap/<name>/<key>`, `Mutex/<name>`) and for the Argo controller's
  parallelism (`Parallelism/workflow-controller`: waiters are postponed
  Workflows, holders the Oberth Workflows Argo admitted; see
  [where limits live](where-limits-live.md)), the current holders and waiters
  with how long each has held or waited, and the stored history: holder and
  waiter intervals, `max_concurrent_holders` (with when), and wait counts,
  maximum, total and median. `max_concurrent_holders: 1` over a window proves
  two runs never overlapped on that lock, at the sampling resolution of
  `agentEvents.interval`. The history keeps the newest
  `agentEvents.syncRetention` intervals. A scoped uplink sees only its own
  runs.
- `promote {..., prune_source: true}`: see below.
- `run_get` (summary by default) and `run_get {summary: false}` include `retry_hint`.

## Safe land: promote with prune_source

`promote` with `prune_source: true` (single source into the default branch)
records the request with the promotion. When the promotion is terminal Oberth:

1. keeps the branch unless the promotion **passed**;
2. reads the upstream target and keeps the branch unless it **equals the
   promotion's result SHA** (a transient read failure retries on the next
   sweep, for at most an hour);
3. plans the deletion of exactly `{source_branch, source_sha}` with the same
   merged check as `branch_delete_many`, so a moved head or an unmerged source
   is kept;
4. deletes it on the mirror and queues the upstream deletion.

The outcome (`pruned` or `kept` with the reason) is audited as
`promotion.prune_source.pruned|kept`, shown in `promote_status` as
`prune_source`, and carried by the `promotion.finished` event, which is
emitted only after the decision. A failed promotion can never be followed by a
prune.

`branch_delete_many` (CLI `oberth branch prune`) keeps refusing unmerged
branches unless `allow_unmerged` is set, and now also needs a `reason`
(`--reason`) to confirm a plan with unmerged entries; the reason and the
number of unmerged branches are recorded in the batch audit.

## Retry hints

`retry_hint` classifies a run that did not pass from its own durable fields:

| `class` | `cause` | Suggested `action` |
|---------|---------|--------------------|
| `infrastructure` | `git_transport` | `run_rerun` (retries only the publication); while Oberth's automatic retry runs, `publication` shows `attempt`, `max_attempts` (5) and `next_retry_at` |
| `infrastructure` | `eviction`, `executor_identity`, `startup_deadline`, `infrastructure` | `run_retry` |
| `infrastructure` | `image_pull`, `deadline_artifact` (a deadline reported below 90% of the step budget), `interrupted` | `run_rerun` |
| `infrastructure` | `auto_retried` | none; follow `auto_retried_by` |
| `code` | `step_failed`, `deadline`, `oom`, `resource_limit`, `upstream_rejected` | fix the code, the git resources or the upstream |
| `unknown` | `unknown` | read `run_get` and `run_logs` |

Upstream publication already retries transient transport failures
automatically: five attempts over about thirty minutes with exponential
backoff. Each scheduled retry is a `publication.failed` event with
`final: false`.

## Values

Each value is a serve flag with the same default; the defaults and their
bounds are in `charts/oberth/values.yaml` under `agentEvents` and in
`oberth serve --help`.

| Value | Flag | Meaning |
|-------|------|---------|
| `agentEvents.retention` | `--agent-events-retention` | newest events kept |
| `agentEvents.syncRetention` | `--agent-events-sync-retention` | newest lock intervals kept |
| `agentEvents.interval` | `--agent-events-interval` | sweep period |
| `agentEvents.lockWaitThreshold` | `--agent-events-lock-wait-threshold` | wait before `run.lock_wait` |
| `agentEvents.maxStreamsPerToken` | `--agent-events-max-streams-per-token` | open GET streams per credential |
| `agentEvents.maxBacklog` | `--agent-events-max-backlog` | events replayed on connect |
| `agentEvents.heartbeat` | `--agent-events-heartbeat` | stream keepalive |
