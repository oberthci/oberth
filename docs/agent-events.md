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

Connect Claude Code once per machine with the Oberth plugin (see
[MCP setup](mcp-setup.md#2-configure-claude-code)): the plugin carries the
server entry with `"timeout": 630000` and the token in Claude Code's
credential store, `/oberth:connect` or `oberth client setup` trusts the CA
through `env.NODE_EXTRA_CA_CERTS` in `~/.claude/settings.json`. No environment
variable, token file or launcher script is needed.

**The default loop, in every session: one `events_wait` kept open.** While a
session has runs in flight it keeps one call open:

```
events_wait {kinds: [run.held, run.finished], own: true, after_id: <cursor>, timeout_s: 600}
```

The call blocks server-side until one of the caller's own runs holds or
finishes. In an interactive session Claude Code moves an MCP call that runs
longer than 120 s into a background task
(`CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS`) and delivers its result when it
returns, so the agent keeps working and the result wakes the session: a held
run is serviced within seconds, long before `abandon_at`. `-p` mode and the
Agent SDK wait for the call in the foreground. On a result, or on
`timed_out`, the agent acts on the events and calls again with
`next_after_id` as `after_id`; it stops when nothing is in flight. Without
`after_id` the call waits for the next event only, so the first wait starts
before the push, or the run is read once with `run_get` after it.

Events carry an increasing `id`. An agent remembers the highest one it has
handled and skips anything at or below it, which makes a pushed copy of the
same event (below) harmless. The plugin's `oberth-agent-loop` skill and the
server's MCP instructions both state this loop, so a session follows it
without being told.

`run_wait {id, timeout_s: 600}` is the same wait for one run. Verified with
Claude Code 2.1.322 in `-p` mode: a `run_wait {timeout_s: 600}` on a running
run returned `state: timeout` after 10 m 0 s, with no client timeout. After a
reconnect or a long gap, `events_since {after_id}` reads what happened in
order.

The 630 s timeout matters here: Claude Code's default per-request timeout for
an HTTP MCP server is 60 s, which cuts a 600 s wait at one minute. The
plugin's timeout covers the longest wait plus its final heartbeat. An entry
written by hand needs the same field.

**Optional push: interactive sessions started with the development-channels flag.**

```sh
claude --dangerously-load-development-channels server:oberth
```

`server:oberth` names a server entry registered by hand under that name; for
the plugin, Claude Code's form is `plugin:oberth@oberthci`.

Claude Code's channels are a research preview. A session receives channel
notifications only from servers named for that session: `--channels` accepts
only plugins on Anthropic's channel allowlist (or an organisation's
`allowedChannelPlugins` managed setting), and any other server, Oberth
included, needs `--dangerously-load-development-channels`, which shows a
confirmation dialog at each start. The flag is ignored in `-p` mode and in the
Agent SDK, and no settings key, environment variable or `.mcp.json` field
replaces it. With a hand-registered entry, Claude Code 2.1.322 logs `Channel
notifications registered` for `oberth` over HTTP and holds one of the
credential's GET streams (the same has not been checked through the plugin's
entry); events then arrive in the session as
`<channel source="oberth" kind="run.held" step="test" abandon_at="..." ...>`.
Confirm on your client version with `/mcp` and `claude --debug`. The
`events_wait` loop stays the protocol either way; a pushed event only arrives
a little earlier, and its `id` lets the agent skip the copy.

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
- `promote {..., prune_source: true}` and `promote {..., tag_after: {name, message}}`: see below.
- `run_list {ref_glob: "land/*", latest: true}` (CLI `oberth runs --ref-glob 'land/*' --latest`):
  one row per matching repository and ref, its newest run, across every
  repository the caller's uplink scope reaches. `ref_glob` uses the same
  pattern syntax as `refs_glob` (`*` never crosses a slash) and also narrows
  the ordinary list.
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

## Tag after promotion

`tag_after` refuses release-version names beginning `v<major>.<minor>.<patch>`, including prereleases and build suffixes, and explicit administrator release routes. Push release tags through Oberth so release CI runs.

`promote` with `tag_after: {name, message}` (single source or train) records
the request with the promotion; a malformed name, an empty message or a tag
that already exists is refused before admission. It needs an admin uplink: a
promotion grant or a repository's promotion policy covers the promotion, not
a new upstream tag. The tag is pushed straight to the upstream; it is not a
release. Its verified publication starts an ordinary tag CI run through the
normal tag admission path, with `release=false` and `.oberth/build.yaml`. When the promotion is
terminal Oberth:

1. pushes nothing unless the promotion **passed**;
2. reads the upstream target and pushes nothing unless it **contains the
   promotion's result SHA**, and also verifies fresh upstream default-branch ancestry;
3. creates an annotated tag (tagger `Oberth <oberth@localhost>`) on the result
   commit and pushes `refs/tags/<name>` without force, verifying the remote ref;
4. records the tag receive and queues its CI run atomically. A stable event ID
   preserves one run through retries and a crash during settlement.

A transient upstream failure retries on the publication schedule
(`pushretry`: at most 5 attempts, backing off from 2 minutes); a tag of that
name that already exists is never moved or reused, and a tag the upstream
refused does not stay on the mirror. The outcome (`pushed` with
the tag object, or `failed` with the reason) is audited as
`promotion.tag_after.pushed|failed`, shown in `promote_status` as `tag_after`,
and carried by the `promotion.finished` event, which waits for it. Use it for
workflows that act on a tag pushed after merge, instead of a client-side
script that polls the promotion and then tags. The `pushed` outcome means the
tag exists and CI was admitted, not that this new CI run has passed; its run ID
is in the reason line. See [tag run behavior](tag-runs.md):

```json
{"repo": "github/acme/infra", "branch": "main", "sha": "<tested sha>",
 "tag_after": {"name": "apply/2026-11-11", "message": "apply the promoted plan"}}
```

`branch_delete_many` (CLI `oberth branch prune`) keeps refusing unmerged
branches unless `allow_unmerged` is set, and now also needs a `reason`
(`--reason`) to confirm a plan with unmerged entries; the reason and the
number of unmerged branches are recorded in the batch audit.

## Retry hints

`retry_hint` classifies a run that did not pass from its own durable fields:

| `class` | `cause` | Suggested `action` |
|---------|---------|--------------------|
| `infrastructure` | `git_transport` | `run_rerun` (retries only the publication); while Oberth's automatic retry runs, `publication` shows `attempt`, `max_attempts` (5) and `next_retry_at` |
| `infrastructure` | `image_pull`, `deadline_artifact` (a deadline reported below 90% of the step budget), `interrupted` | `run_rerun` |
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
