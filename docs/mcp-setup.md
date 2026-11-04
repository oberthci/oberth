# MCP setup

Connect an AI agent (Claude Code or any MCP client) to Oberth.

## Prerequisites

- Oberth is deployed and has at least one upstream registered
- `kubectl` access to the Oberth namespace

## 1. Create an uplink

Run inside the Oberth pod:

```bash
kubectl exec -i -n oberth deploy/oberth -- \
  oberth uplink add - operator@host < ~/.ssh/id_ed25519.pub
```

Replace `operator@host` with a descriptive identity for this uplink.

The command prints three lines exactly once. Save both values:

```
TLS certificate fingerprint: SHA256:abc123...
Uplink token for operator@host (shown once):
oberth_xxxxxxxxxxxxxxxx
```

The token is never stored or recoverable. If lost, create a new uplink.

### Limit an uplink to some upstreams

An uplink normally sees every repository. `--scope` limits it to the
repositories of named upstreams, for example a development machine that may see the
`github` upstream and nothing else:

```bash
kubectl exec -i -n oberth deploy/oberth -- \
  oberth uplink add --scope github - developer@work < developer.pub
```

A scope is an upstream name as `oberth upstream list` shows it; repeat
`--scope` or separate names with commas for several. A scoped uplink:

- sees runs, logs, artifacts, issues, promotions, grants and repositories of
  its upstreams only. Lists, counts, pages and `system_status` cover only
  those; anything else answers exactly like something that does not exist;
- clones, fetches and pushes over SSH only repositories of its upstreams, and
  cannot register a new repository elsewhere;
- files manual issues under its upstream (`issue_create` takes `upstream`,
  required when the scope names several). Issues filed under no upstream are
  not shown to it;
- cannot use the `secretstore_*` tools or `audit_list`, even with `--admin`. Its other admin
  tools act inside the scope only.

Change or remove the limit later; the token and key stay valid:

```bash
kubectl exec -n oberth deploy/oberth -- oberth uplink scope --set github,acme developer@work
kubectl exec -n oberth deploy/oberth -- oberth uplink scope --clear developer@work
```

`oberth uplink list` shows each uplink's scope (`all` when unscoped), and
`uplink rotate` and adding the same key again keep it. Scopes name upstreams:
renaming or removing an upstream narrows every uplink scoped to it, which then
reaches nothing there until its scope is set again. A scoped uplink pages
`run_list` and `promotion_list` with `next_cursor`, a record ID, instead of a
sequence number.

## 2. Configure Claude Code

`oberth install` offers to write this for you on an interactive install, right
after it registers the uplink, into `~/.config/oberth/mcp.json`. What follows is
the same thing by hand, and the explanation of why it is shaped the way it is.

### Keep the token out of the file

The obvious configuration puts the bearer token in a literal header, and then a
credential lives in a file for as long as the file does. `headersHelper` names a
command that prints the headers instead, run fresh on every connection:

```json
{
  "mcpServers": {
    "oberth": {
      "type": "http",
      "url": "https://oberth:30443/mcp",
      "headersHelper": "printf '{\"Authorization\":\"Bearer %s\"}' \"$(security find-generic-password -s oberth-token -w)\""
    }
  }
}
```

That is the same command `OBERTH_TOKEN_COMMAND` names for the CLI, so one
secret source serves both clients and neither configuration file holds a
credential. Substitute your own: `secret-tool lookup service oberth`,
`pass show oberth/token`, `op read op://vault/oberth/credential`.

Three things worth knowing before relying on it. The command runs in a shell
with a ten second budget and its result is not cached, so a keychain read is
comfortable and an interactive unlock may not be. A project- or local-scope
server runs the helper only once the folder is trusted. And on a 401 or 403 the
helper is re-run once and the call retried, so a rotated uplink token recovers
without editing anything.

`headersHelper` is Claude Code's rather than part of MCP. A client that does not
implement it needs the literal header below.

### With a literal token

Add to `.claude/settings.local.json` (user-scoped, never committed) or user-level
config. **Do not** place bearer tokens in `.claude/settings.json` — that file
is typically checked into the repository.

### Via the HTTPS NodePort

```json
{
  "mcpServers": {
    "oberth": {
      "type": "url",
      "url": "https://<node-address>:30443/mcp",
      "headers": {
        "Authorization": "Bearer oberth_xxxxxxxxxxxxxxxx"
      }
    }
  }
}
```

This is the shipped access path — Oberth exposes fixed NodePorts and carries
no tunnel subsystem. If you front the NodePort with your own TLS-terminating
proxy (an ingress, a Cloudflare Tunnel you operate), point the URL at that
hostname instead; a publicly trusted certificate there removes the
client-side trust step below.

Direct access uses Oberth's own CA. With a production secret store
(`oberth install --install-secretstore`) the server serves a 30-day leaf
issued by a CA that lives in OpenBao's PKI engine and renews it in place;
clients pin the CA, which stays the same across renewals. The CA is
name-constrained to the server's DNS names, and the leaf covers the in-cluster
names, `localhost`, `127.0.0.1`, the address the installer advertises and every
`--tls-extra-dns-name` / `--tls-extra-ip`. Reaching the server on any other
address is a hostname mismatch that no trust configuration repairs, so name
the address first.

Fetch the CA (an interactive install writes it to `~/.config/oberth/ca.crt`
and later installs keep that file current):

```bash
kubectl exec -n oberth deploy/oberth -c oberth -- /usr/local/bin/oberth admin read-file \
  /run/oberth-identities/server/ca.crt /run/oberth-identities/server/tls.crt \
  > ~/.config/oberth/ca.crt
openssl x509 -in ~/.config/oberth/ca.crt -noout -fingerprint -sha256
```

Claude Code reads extra trust anchors from `NODE_EXTRA_CA_CERTS`. Set it in
`~/.claude/settings.json` so every launch, including terminal multiplexers,
sees it:

```json
{ "env": { "NODE_EXTRA_CA_CERTS": "/home/you/.config/oberth/ca.crt" } }
```

`claude mcp list` should then show the server as connected. For other clients,
add the CA to the system trust store instead (`update-ca-certificates` on
Debian/Ubuntu, `security add-trusted-cert` on macOS); the name constraints
keep it from vouching for any other host.

## 3. Verify the connection

After restarting Claude Code, the MCP server appears in the tool list. Verify
by asking Claude Code to call any Oberth tool, or test manually:

```bash
curl --fail-with-body --silent \
  --cacert ~/.config/oberth/ca.crt \
  -H "Authorization: Bearer oberth_xxxxxxxxxxxxxxxx" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
  "https://localhost:30443/mcp"
```

A successful response lists the server's tools; `oberth version` and this
server's own `status` tool are the authorities on how many it should be, since
the count moves with the release. Behind an operator-run TLS-terminating
proxy with a publicly trusted certificate, drop `--cacert` and `--resolve` and
call the proxy hostname directly.

## MCP tools

| Tool | Description |
|------|-------------|
| `status` | CI status for a SHA or branch, including the failed step |
| `logs` | One named step's log output for a SHA, optionally filtered |
| `run_resources` | Read-only resource history, numeric git declarations and p95/max suggestions; repo or exact run, ref/default/any, last 1..100, include_failed, JSON or YAML; see [measured requests](measured-requests.md) |
| `run_get` | One durable run and its steps by full run ID or unique 8+ character prefix; `summary:true` returns repository/ref/SHA, admission context, superseded status, failure and compact steps |
| `run_retry` | Requeue the newest interrupted or infrastructure-failed branch run using its same repo/ref/SHA; workflow-level failures without a failed step qualify, while test failures and older runs are refused |
| `run_rerun` | Rerun a failed or interrupted run at the same SHA and ref (`id`, optional `reason`, admin-only `force`), linked by `rerun_of`: covers failed steps (flaky tests), non-newest runs and tag/release runs. Refused while the ref has an active run, once a branch moved past the SHA, for promotion/plan/apply runs, and for a tag that is already published or green or whose release failed only in publication (use `publish_retry` on that run). A publishing-only failure is re-pushed as `publish_retry` does instead of re-run. Non-admins: branch-push runs; tags need admin and `force`. Audited as `run.rerun` |
| `run_cancel` | Cancel one queued, waiting or running run (audited; `id`, optional `reason`, admin-only `force`). The run becomes `interrupted` with phase `cancelled` and a reason naming the caller; a claimed run's Workflow is deleted and its admission released at once; it is never re-fired and opens no CI issue. Non-admins: branch-push runs outside a concurrency group only. Protected priority runs can be cancelled only by their requester or an unscoped admin; the existing run-cancel policy still applies. Admins: any run, with `force` for tag/release and apply runs. Cancelling promotion CI fails the promotion without merging. Terminal runs are refused |
| `run_purge` | Purge retained logs, artifacts or both for a terminal run; requires an unscoped admin uplink and `id`, `what`, `reason`. Active runs and protected release/promotion/issue/grant evidence are refused. Run/step history remains; audited purge markers identify the actor and date |
| `run_logs` | One exact burn/step log by full run ID or unique 8+ character prefix, optionally filtered |
| `artifacts` | The files a run kept, with size and modification time |
| `artifact_get` | One artifact's contents by run ID and name, optionally filtered |
| `wait` | Long-poll until a SHA or exact run ID reaches a terminal state |
| `sync` | Park a WIP branch upstream without a green gate (not completion evidence) |
| `queue_cap_config` | Unscoped admin only: set `queueCap.maxNotStarted` with `maxNotStarted` (integer, 0 disables; off by default). Defer branch run creation when queued and waiting runs reach the cap; drain deferred receives in arrival order after capacity opens. Accepted deferred pushes stay in Oberth's Git cache and durable receive journal; automatic upstream publication requires successful CI. Tags/releases, promotions and schedules bypass the cap. Audited; setting and backlog survive restart. `system_status.queueCap` reports the cap, count, deferred count and reason |
| `promote` | Promote a green branch SHA or an authorized parked SHA; parked sources always run merged-tree CI; push without force |
| `promotion_list` | Recover durable promotion IDs by exact repo/full source SHA/status; bounded pages with an admission-sequence cursor |
| `promote_status` | Wait for a promotion record to become terminal; a failed or interrupted promotion that was retried names its successor in `retried_as` |
| `promote_retry` | Retry a failed or interrupted promotion (`id`): a new promotion of the same repo/source SHA/target, merged onto the target's current head and re-tested; for ordinary green branch sources, a transient push-only failure reuses its exact tested SHA when the target is unchanged; parked sources run promotion CI again. Same authorization as `promote`; the original grant still authorizes it, because a grant is consumed only by a delivered promotion. Audited as `promotion.retry` |
| `publication_abandon` | Admin only, unscoped: abandon a pending publication by full publication `id` with a required `reason`. The audited transaction marks the intent `abandoned`, its run `failed/abandoned`, and its promotion failed. It never pushes; earlier ambiguous pushes may already be upstream. Terminal intents are refused and abandoned intents cannot be reactivated. CLI: `oberth publication abandon <publication-id> --reason "stop delivery"` |
| `publish_retry` | Retry upstream publication for a branch or tag run whose burns passed but whose upstream push failed, including exhausted infrastructure failures. Transient errors get five durable attempts with 2/4/8/16-minute backoff, and pending responses say queued; a promotion is retried with `promote_retry` instead |
| `issue_create` | Create a manual issue, installation-wide or filed under one `upstream` |
| `issue_get` | Get an issue by ID; optional `fields` and `body_limit` reduce the response |
| `issue_get_many` | Get 1..50 issues in request order, with optional `fields`/`body_limit` and explicit missing/size-limit results |
| `issue_update` | Update an issue title and body or move a manual issue with `upstream`; optional `expected_body_sha256` rejects the update on mismatch; returns a compact acknowledgment |
| `issue_append` | Append text to an issue body with a dated separator (no full-body re-send); returns a compact acknowledgment |
| `issue_close` | Close an issue; returns a compact acknowledgment |
| `issue_reopen` | Reopen a manual issue by ID; an already-open issue is unchanged, CI issues are rejected |
| `issue_delete` | Delete an accidentally created manual issue |
| `issue_list` | List issue metadata with repo/kind/state/query filters, total and faceted counts, and 1..50 records per page |
| `issue_unlock` | Admin-only audited lock break, scoped to visible issues |
| `issue_close_many` | Admin-only closure of 1..50 unique IDs with a reason and per-ID results; honors locks |
| `issue_lock` | Acquire or renew a five-minute caller-owned issue lock |
| `uplink_list` | Unscoped-admin-only public credential lifecycle metadata for all registrations |
| `uplink_revoke` | Unscoped admin-only audited SSH and bearer revocation; protects the last active admin |
| `uplink_rotate` | Unscoped admin-only handoff to an already-issued matching replacement key, atomically revoking the old key |
| `access_dismiss` | Unscoped-admin-only exact pending-declaration dismissal |
| `run_kvm_preflight` | Deprecated; read the retained trusted preflight journal for one run |
| `kubevirt_grant` | Deprecated; audited administrator grant for the existing bounded VM path |
| `kubevirt_revoke` | Deprecated; revoke a repository's old VM grant |
| `kubevirt_grant_get` | Deprecated; read one repository's old VM grant |
| `kubevirt_grant_list` | Deprecated; list old VM grants within the caller's scope |
| `secretstore_sync_receipt` | Record the receipt for a reviewed secret-store synchronization plan |
| `promote_grant` | Issue an audited grant for a source SHA and promotion target |
| `promote_grant_revoke` | Revoke a promotion grant |
| `promote_grant_list` | List promotion grants within the caller's scope |
| `workflow_capability_grant` | Audited administrator grant for operator-owned namespaced resource access and extended resources |
| `workflow_capability_revoke` | Revoke the repository's operator capability; blocks new authorization |
| `workflow_capability_get` | Read the repository's current operator capability |
| `access_list` | List secret access grants for a repository |
| `access_allow` | Grant a step access to a secret path (admin-only) |
| `access_revoke` | Revoke a step's access to a secret path (admin-only) |
| `resource_grant` | Set per-container numeric `requests` and `limits` ceilings for CPU and memory (Kubernetes quantity strings); optional ephemeral-storage defaults to 16Gi; admin-only, audited |
| `resource_revoke` | Return numeric ceilings to requests/limits of 4 CPU, 8Gi memory, 16Gi ephemeral-storage; admin-only, audited |
| `resource_grant_get` | Read the numeric resource ceiling and grant/revocation provenance |
| `size_grant` | Set a repository's maximum admission size, `L` or `XL` (admin-only, audited); without a grant the maximum is `L`; re-enables the oldest waiting tag's automatic boost within that maximum |
| `size_revoke` | Return a repository to `L` and disable automatic tag priority, even if never granted (admin-only, audited) |
| `size_grant_get` | Read a repository's maximum admission size (`max_size`) and grant state: `never_granted`, `granted`, or `revoked` |
| `queue_priority_config` | Replace administrator RC globs with `rc_patterns` (e.g. `["rc/*", "release/*"]`); `[]` disables; unscoped admin, audited |
| `queue_priority_get` | Read RC patterns and the 60-minute threshold after which a standard run ages into promotion; unscoped admin |
| `priority_grant` | Allow `identity` to request priority up to `max_class` (`promotion` or `high`; legacy `highest` aliases high) until RFC3339 `expires`; unscoped admin, audited |
| `priority_grant_list` | List priority grants and expiry; unscoped admin |
| `priority_grant_revoke` | Revoke an identity's grant for future submissions; accepted builds keep their priority; unscoped admin, audited |
| `run_priority` | Set a waiting run using exact `run_id`, `priority` (`low`, `standard`, `promotion` or `high`; legacy aliases accepted) and `reason`; unscoped admin, audited |
| `concurrency_groups` | Read YAML-defined shared groups, limits, holders and waiters, including idle groups (unscoped, read-only) |
| `admission_hold` | Admin-only [admission hold](admission-hold.md) with allowed classes and bounded expiry; read through system_status admission.hold. |
| `admission_release` | Release one admission reservation no owner will release, typically a granted one held for a terminal run whose owner hung; deletes the Workflow first as proof, `force` without it (admin-only, unscoped, audited) |

For `access_allow` and `access_revoke`, `secret` is the exact path recorded in
`access_list` or the pipeline declaration, including the mount and KV API prefix
when present. For example, `oberth/data/release/cosign-secret` is a different
identity from `release/cosign-secret`; the server does not expand or normalize
one into the other. Copy the listed `repo`, `step`, and `secret` tuple when
revoking a grant. The grant ID is returned as evidence, not accepted as a revoke
selector. `step: "*"` selects the wildcard grant itself; it does not revoke
named-step grants. A missing exact tuple is refused without changing grants.
Revocation removes admission approval immediately; complete the documented
`oberth secretstore sync` procedure to remove the corresponding Bao policy read.

Run-ID arguments accept the 8-character run ID printed after a push, or any
unique hexadecimal prefix of at least 8 characters. Uniqueness is checked only
among runs the caller is authorized to see. An ambiguous prefix returns the
matching full IDs; use a longer prefix or the full 32-character ID. Responses
always include that full ID.

For `run_logs`, an explicit `step` without `burn` selects that step's burn if
its name is unique across the run's recorded results or active progress. An
ambiguous name lists the candidate burns and requires `burn`; an unknown name
returns not-found. If both are omitted, the failed burn/step remains the default.
An explicit burn without step defaults only when that burn has exactly one step.

`run_list` accepts `status` as one existing display status or a non-empty list:
`queued`, `running`, `passed`, `failed`, `cancelled`, `superseded`,
or `infra-retried`. `repo` and `ref` remain
optional exact filters. Filtering precedes the page limit; queue-first ordering,
active-row limits and terminal cursors are unchanged. Send the same filters with
the returned cursor. REST accepts repeated or comma-separated `status` values.

```sh
oberth mcp call run_list '{"status":"queued","repo":"project"}'
oberth mcp call run_list '{"status":["failed","infra-retried"],"limit":25}'
oberth mcp call run_logs '{"id":"01234567","step":"compile"}'
```

Promotion selectors use the same 8+ character unique-hex-prefix rule as runs,
including `promote_status`, `promote_retry`, `publish_retry` and promotion-list
cursors. Ambiguity is checked within the caller's visible repositories and names
the matching full IDs. Long polls pin the resolved full ID on their first read.

For an active Argo step, retained output may not exist until the step completes.
`run_logs` and `logs` then return a `step log is pending` error with the run and
step identity and retry guidance. Available retained output remains readable
with the usual filters; an unknown step still returns not-found.

To wait for one exact run, pass `{"id":"<run ID or unique 8+ character prefix>"}`.
The `id` argument cannot be combined with `repo`, `sha`, or `trigger`.

To wait for a release, pass `{"sha":"<commit-or-tag-object-sha>","trigger":"release"}`.
`release` and `tag` select the tag-triggered run, even if a newer branch or
promotion names the same commit. Completed releases return
`still_running: false`; a timeout before a matching release exists keeps
`still_running: true`. Use `repo` if the SHA identifies runs in several repositories.

### Purging retained evidence

An unscoped admin can call `run_purge` with
`{"id":"<run-id>","what":"both","reason":"remove retained output"}` or run:

```sh
oberth run purge <run-id> --what both --reason "remove retained output"
```

`what` is `logs`, `artifacts` or `both`. A printable, one-line reason is
required (at most 512 bytes). Active executions, release/tag/plan/apply
runs, and evidence needed by promotions, trusted plans/applies, open CI issues
or active promotion grants are protected. There is no force override.

The run and its step results remain visible. `run_get` includes durable
`Purges` markers; reads of purged evidence report who purged it and when
(HTTP 410). A pending or failed attempt reports unconfirmed completion and
can be retried. The periodic retention sweeper uses the same protections;
its policies and budgets are configured with the chart's `retention` values.

| Chart value | Default | Server flag and behavior |
|-------------|---------|--------------------------|
| `retention.interval` | `10m` | `--retention-sweep-interval`: positive interval between sweeps, independent of artifact collection |
| `retention.keepNewestPerRef` | `1` | `--retention-keep-newest`: newest terminal runs kept per repository/ref; `0` disables this extra protection |
| `retention.logs.maxAge` | `0s` | `--log-retention`: age since run completion; `0s` disables age retention |
| `retention.logs.limitBytes` | `67108864` | `--run-log-limit-bytes`: maximum retained bytes per run; exceeding it fails the run |
| `retention.logs.budgetBytes` | `0` | `--logs-budget-bytes`: total log budget; `0` disables size retention |
| `retention.artifacts.maxAge` | `0s` | `--artifact-retention`: age since run completion; `0s` disables age retention |
| `retention.artifacts.limitBytes` | `268435456` (256 MiB) | `--artifacts-limit-bytes`: maximum collected artifact bytes per run |
| `retention.artifacts.budgetBytes` | `4294967296` (4 GiB) | `--artifacts-budget-bytes`: total artifact budget; `0` disables size retention |

Age values use Go durations, for example `720h` for 30 days. Sweeps delete
oldest eligible evidence when either its age or the global storage budget is
exceeded. Active runs, protected evidence, and the newest terminal runs per ref
can keep storage above the budget. Manual purges keep the same evidence
protections but may remove the newest terminal run. The separate
`admission.diskFloorBytes` value controls the free-space admission floor.

When an evidence class has an enabled age or budget policy, sweeps also reclaim
orphaned files with no run row, recording the removal in the audit trail.
Disabled policies preserve orphaned files too.

`system_status` reports storage usage and free space cached by the last sweep,
the disk floor, and the last sweep time/error. It does not rescan evidence trees.

### Filtering log output

`logs`, `run_logs` and `artifact_get` accept five optional parameters. Filtering happens on the
server, so a narrowed read never sends the whole step over the wire.

| Parameter | Meaning |
|------|-------------|
| `pattern` | RE2 expression; only matching lines are returned |
| `context` | Lines of context on each side of a match (max 50) |
| `offset` | First line to return, 0-based. Pages over matches when `pattern` is set, over raw lines otherwise |
| `limit` | Maximum lines returned. Omit for no line cap |
| `tail` | Take from the end of the step rather than the start |

`pattern` matches against the line with its `[burn/step]` prefix removed, so `^`
and `$` anchor to the output a step actually produced. The returned bytes keep
the prefix.

Every response carries counts so a narrowed read is never mistaken for a
complete one:

| Field | Meaning |
|------|-------------|
| `total_lines` | Lines in the step before any filtering |
| `matched_lines` | Lines matching `pattern`; equals `total_lines` without one |
| `returned_lines` | Lines actually in this response |
| `truncated` | Whether a limit or the response ceiling withheld anything |
| `bytes` | True size of the full step range |
| `line_numbers` | Original position of each returned line |

A step above the 4 MiB response ceiling returns a truncated result with
`truncated: true` rather than failing. Prefer a pattern to retrieving a whole
step: a single step can exceed a model's context window.

| `repo_usage` | Bounded external resource report by repository/step: sampled p50/p95/max, requests, coverage and used/request ratios. |
| `repo_list` | List registered repositories with upstream and probe state |
| `repo_archive` / `repo_unarchive` | Archive or restore a repository, preserving history (unscoped admin only); archived repositories refuse new grants |
| `repo_remove` | Remove a repository mapping and its Git cache (admin-only) |
| `run_list` | List runs with repository/ref/SHA, size/declared demand, queue position and superseded status; optional repo/ref, display-status (one value or list) and `superseded:true` filters (bounded page) |
| `audit_list` | Admin-only: verified audit-chain entries newest first, filtered by action prefix, actor, repo and time range (paged) |
| `schedule_list` | Schedules declared in `.oberth/schedule.yaml`: cron, next run, last outcome, pause (scoped uplinks see their repositories) |
| `schedule_pause` | Admin-only: pause one schedule entry (audited `schedule.pause`) |
| `schedule_resume` | Admin-only: resume a paused entry (audited `schedule.resume`) |
| `schedule_trigger` | Admin-only: run one entry now through normal CI admission (audited `schedule.trigger`) |
| `system_status` | System health: database, upstreams, cluster, audit, version |
| `secretstore_plan` | Admin-only read-only: compute per-repo policy/role diff from approved grants; never writes policies |
| `secretstore_verify` | Admin-only real secret-store preflight using configured server or per-repo release/CI identity; paths/key counts only by default |

## Audit log

`audit_list` (admin-only; `oberth audit` on the command line) reads the
hash-chained audit log that every mutating tool writes to. Filters: `action`
(prefix, for example `secret_access.`), `actor` (exact identity), `repo` (rows
about the repository, its runs and its promotions), `since` and `until` (RFC
3339 or a duration such as `24h` meaning that long ago), `limit` (default 50,
maximum 200) and `before` (the previous page's `next_before`). Entries come
newest first. Each carries its chain position (`id`, `sha256`,
`previous_sha256`), and the server recomputes its hash before returning it; an
entry that does not verify fails the call. `head_id` is the chain's current
head.

A scoped uplink gets no audit rows at all, even with `--admin`: audit rows span
uplinks, upstreams and the secret store, and filtering them per scope would be
guesswork.

`oberth audit --tail` prints the newest entries oldest first, then follows new
ones, paging through bursts larger than 200 matches without gaps or repeats.
`GET /api/audit` accepts the same filters and returns the same JSON, with an
`X-Oberth-Next-Before` header on full pages. It requires an unscoped admin.
`oberth audit head` and `oberth audit record` run in-pod.

Lifecycle records. `oberth backup` records `backup.completed` (archive SHA-256,
bytes, scope and the snapshot's audit head) and `oberth restore` records
`restore.completed` (archive SHA-256, parts, the archive's audit head).
`oberth upgrade` records `upgrade.completed` (`from`, `to`) after rollout and
version verification. These commands run `oberth audit record` in the server pod
once the operation has succeeded. The server cannot observe these itself, so a
failure to record (an older server, an
unreachable admin daemon) is a printed warning; the completed operation stays
successful. Dry runs, failed upgrades and already-current upgrades write no
completion record. `restore.started` cannot survive replacing the database.

`system_status` and `GET /api/status` include `last_backup` after a successful
backup record: `at` is its timestamp, `head` contains the archive snapshot's
`id` and `sha256`, and `archive_sha256` identifies the archive. Later audit
appends do not change the snapshot head. The field is omitted before any backup
record and for scoped callers. A latest backup record whose details cannot be
read omits `last_backup` and reports `last_backup_warning` instead; `database`
stays `ready`.

## Schedules

Schedules are declared in a repository's `.oberth/schedule.yaml` on its default
branch; a commit changes them. The `schedule_*` tools (and `oberth schedules
list|pause|resume|trigger`, which call them remotely) operate on what is
declared without one:

- `schedule_list {repo?}` lists each entry's `cron`, `next_at`, `last_fired_at`,
  `last_outcome` (`fired`, `skipped`, `failed`, `refused` or `paused`) and, when
  paused, who paused it, when and why. Entries the scheduler refuses are listed
  with the reason. `GET /api/schedules?repo=<repo>` returns the same JSON; omit
  `repo` to list all visible repositories. Both reads require authentication,
  and a scoped uplink sees its own repositories only.
- `schedule_pause {repo, entry, reason}` holds an entry: each tick at which it
  comes due records outcome `paused` instead of firing. `schedule_resume
  {repo, entry}` lifts the hold; an entry that came due meanwhile fires at the
  next tick, once. Both are admin-only, change nothing when the entry is
  already in that state, and are audited (`schedule.pause`, `schedule.resume`
  on resource `<upstream/org/repo>#<entry>`). The audit chain is the stored
  state, so there is no separate table and no schema change.
- `schedule_trigger {repo, entry, force?}` queues the entry's run now at its
  branch head with `trigger=schedule`, through the same `EnqueueCI` admission
  path as a cron fire or a push, attributed to the calling uplink. It is
  admin-only and audited (`schedule.trigger`, written once the run is queued
  and naming it in `run_id`, so a refused or failed enqueue records nothing).
  Like the cron path it is refused while the repository has active runs, and
  while the entry is paused, unless `force` is set; a forced run supersedes the
  branch's active runs as a push would. A manual trigger does not count as the
  entry's last fire, so it never moves the cron cadence.

A scoped admin uplink can pause, resume and trigger only entries of its own
upstreams; another upstream's repository answers as if it did not exist.

## Scheduling observations

When a run is queued or running without any running/completed steps, `status`
adds `scheduling`. `run_get` and `/api/runs/{run}` expose the same object as
`Scheduling` alongside the existing `Run`, `Steps`, and `Repository` fields.
The fields are optional; clients should keep displaying durable run status
when an observer is unavailable.

- `observed_at` and `age_seconds` describe when the snapshot was read. `wait`
  retains that timestamp and updates its age before returning. Controller
  state may itself lag; this is diagnostic context, not execution evidence.
- `queue_position` is global accepted order across unclaimed and claimed but
  unadmitted runs, computed before repository/ref filters. Publication
  gates can let later runs execute first; it is not an ETA.
- `admission` reports the actual resource gate reservation: `preparing`,
  `waiting`, or `admitted`, with the same global queue `position`, size, weight and
  aggregate used/capacity. Scoped uplinks see no global position, used or capacity.
  This explains a durable `running` run that is still waiting behind an exclusive
  XL build. A run whose pipeline declares
  `oberth.ci/concurrency-group` also reports `group`; while it waits on that
  group it reports `group_limit`, all `group_holders` and an earlier
  `group_ahead` where applicable. Group names are shared across repositories;
  scoped views omit invisible holder/waiter IDs. The queued reason names the
  group, limit and visible holders. `status` carries the recorded group as `concurrency_group`;
  `run_get` and `run_list` carry it as the run's `ConcurrencyGroup`.
- `execution` reports observed Workflow phase, up to 16 node phase/message
  summaries and 16 Pod phase/reason summaries. It can identify unscheduled Pods,
  insufficient CPU/memory, unbound claims, image pull waits, startup, and known
  Argo synchronization messages. Free-form text is withheld because it can
  contain parameters or credentials; only curated summaries are returned.
- `not_observed` means the exact Workflow was absent at observation time. It
  does not diagnose the cause: source preparation can precede Workflow creation.
  `unavailable` means observation failed or was not configured. Neither changes
  the durable run status. Partial Pod observation failure appears in `pods`.

Observation is read-only, capped at two seconds, and omitted after recorded
step execution or a terminal run. No cluster mutation or extra permissions are
required.

## Run list views

Run rows in `run_list` and `/api/runs` carry `Repo` (repository name),
`Ref`, `SHA`, `size`, `declared_resources`, `QueuePosition` when available,
and the boolean `superseded`. Full run detail carries the same context in
`Run`; compact `run_get` uses `repo`, `ref`, `sha` and `queue_position`. Size
and declared resources come from the current admission reservation, without polling
Workflow/Pod state or reading a checkout. An unknown size is `""` and unknown
demand is `null`, including before the scheduler has read the pipeline and
after the reservation is released. Declared demand is the CPU, memory and Pod
reservation, including helpers/headroom, rather than measured usage. Scoped
uplinks continue to omit global queue positions.

`superseded` is true for a branch run when a run with a greater accepted run
sequence is queued or running in the same lane: repository, ref kind, ref and
schedule entry, as in enqueue supersession. Named schedules are independent of
ordinary branch pushes and of other schedule entries. Legacy unnamed schedules
share the ordinary branch lane. Tags and promotion attempts never qualify and
cannot supersede branch work. A newer terminal run does not qualify.
The signal is independent of priority, SHA equality, admission order and page
boundaries. Priority-protected runs and runs with pending publications can
still match. This is a live read observation, distinct from durable
`SupersededBy` cancellation history; it does not compare with an upstream branch
head or guarantee that a matching run is safe to cancel.

Use `run_list({"superseded":true})` or `/api/runs?superseded=true` to find
matching runs in one read, optionally with `repo`, `ref` and `view`. Omitted or
false leaves the list unfiltered. Filtering happens before the page limit,
while queue positions retain their global rank. To inspect only active work,
use `view:"active"`; results can include running work as well as queued work.
Review each matching run and explicitly call the admin `run_cancel` tool with
its ID and a reason. Listing never cancels work; existing priority/protection
and cancellation authorization checks still apply.

`run_list` and `/api/runs` take an optional `view`. Omitted or `all`, the
list has three groups: the queue in scheduler claim order (highest effective
priority first, including aged promotion work, then FIFO within each class); running runs, most recently started first; then terminal runs, most
recently finished first, with passed and failed interleaved by finish time.
The first page carries every queued and running run; the page limit counts
only terminal rows, so no limit or page ever drops a queued or running run or
sorts it below a finished one. Later pages continue the terminal runs by a
stable end-time-and-ID cursor. A run cancelled before admission is terminal
and sorts by its end time. A pruned cursor returns 404: restart from the first
page. `latest_per_ref=1` keeps the same grouping for each ref's newest run.

- `history` keeps only terminal runs (passed, failed, interrupted), with legacy
  sequence paging via `before`/`next_before`/`next_cursor`.
- `active` is deprecated; it still returns every queued, waiting and running
  run in one unpaged list (at most 500): started runs (oldest start first), then
  all runs that have not started, claimed or unclaimed, in one queue. Effective
  priority comes first, then ascending admission sequence and queue sequence
  (FIFO), matching the scheduler's claim order. Each run that has not started carries
  `Scheduling` with its state, message, queue or admission position, size,
  weight, used/capacity and concurrency group, so a run blocking the head of
  the queue, or one waiting on its group, says so.

Every run response includes `display_status` and `display_reason`, while
durable `Status` stays unchanged. `display_status` is exactly one of
`queued`, `running`, `passed`, `failed`, `infra-retried`, `superseded` and
`cancelled`; everything finer is the reason line. Every run that has not
started is `queued`: durable queued, or claimed before admission (raw
`Status` `waiting`). Its `display_reason` names what it waits for in plain
words (`next in line`, `waiting for 9.9 CPU, 16 GiB`,
`waiting for 1 CPU, 512 MiB · host IO pressure`, `10 runs already running (limit 10)`,
`group deploy busy`, `starting`);
`Scheduling.state` and `Scheduling.message` keep the raw observation.
Admission records `started_at`, so an admitted run, including a trusted
preflight before Workflow submission, displays `running`; an uncertain
Workflow create or an exceeded deadline is its reason, not a status. Passed rows show `closes #N` or `refs #N` only from Git-parsed commit trailers,
optionally followed by `schedule <entry>`. This display is independent of the
repository policy authorizing closure; it cannot close issues by itself.
Terminal runs display
`passed` (including published releases), `failed` (a deadline or memory
kill is the reason), `infra-retried`, `superseded` with the superseding SHA,
or `cancelled` with the cancelling actor or restart action.
After a server restart, a
running branch run whose Argo Workflow still exists is adopted first: it keeps
its run ID and continues to its real result (#1172). Re-queueing remains the
fallback when that Workflow is missing; such restart re-fires carry the durable
reason `re-queued after restart`, which is distinct from supersession even when
the successor has the same SHA. The first upgrade to a release with adoption
still re-queues running branch runs, because the old process does so on its own
SIGTERM; adoption applies from the next restart. Interrupted results neither
open nor resolve CI-red issues. Retry and rerun eligibility continues to use the durable status
and cause. The `status`/`wait` tool's `status: "delivered"` remains the legacy
alias of `passed` for all passed runs; its `display_status` is `passed`.

`oberth runs --active` is deprecated and remains available for compatibility.
The dashboard uses the default flat list, status chips, and an independent
release toggle. `/api/runs/counts` supplies its running/queued header and dated
page range: `Total - Queued`. The response retains `Total` and `ByRepo`, adding
`Queued`, `Running`, `QueuedByRepo` and `RunningByRepo`; all respect uplink scope.

## JSON API

Authenticated `GET` endpoints serve the same state as a web dashboard:

| Endpoint | Content |
|----------|---------|
| `/api/runs` | All CI runs; `?view=history` for terminal runs, `?view=active` for the deprecated ordered active list |
| `/api/runs/counts` | Total retained, queued and running counts, also per repository |
| `/api/repos` | Registered repositories |
| `/api/issues` | Paged issues with `total` for the selected filters and `counts` by state and kind |
| `/api/status` | System health summary |

Unauthenticated: `/healthz` (compatibility/startup), `/livez` (local writer and
background task progress), and `/readyz` (startup and serving readiness).

## Notes

- The MCP endpoint accepts `POST /mcp` with JSON-RPC 2.0 and negotiates
  `2025-03-26` or `2025-06-18`. See [version negotiation](mcp-protocol.md)
  for input validation, fallback, version headers and stateless initialization.
- Bearer tokens are bound 1:1 to an uplink SSH public-key fingerprint and
  identity.
- Run selectors (`ref`, `sha`) are resolved across all repositories; pass
  `repo` only when a short SHA or branch name is ambiguous.
- `sync` parks a branch upstream for visibility. It is not integration or
  completion evidence.
- `promote` requires explicit integration authority, a full SHA, and a named
  target branch. The source must be a green branch commit or an authorized
  parked commit. Parked commits always run promotion CI, including a
  fast-forward. It never force-pushes. See [park promotion](park-promotion.md).

### Long-poll tool timeouts

`wait` and `promote_status` long-poll until the target reaches a terminal
state or the server-side `timeout` expires (maximum 600 seconds). The server
extends its HTTP write deadline for these tools, so a large `timeout` does not
hit the server's own `WriteTimeout`.

Clients that explicitly accept `text/event-stream` receive a Streamable HTTP
SSE response for these two tools. Authentication, strict argument validation,
and the first target lookup complete before the stream begins. Fixed comment
heartbeats are flushed immediately and every 20 seconds while the observation
waits. The stream ends after one `message` event containing the normal JSON-RPC
response with the original request ID. Headerless clients, JSON-only clients,
and clients excluding SSE with `q=0` retain the JSON response.

Heartbeats prevent an idle reverse proxy from timing out a silent long poll.
The observed Cloudflare HTTP 524 at approximately 125 seconds is one such
failure; extending the origin write deadline alone does not prevent it.
Heartbeats do not extend a client's total MCP request deadline. Keep the tool's
`timeout` below that configured deadline (for Codex, `tool_timeout_sec`). JSON
clients behind an idle proxy should use shorter waits and retry; 100 seconds
is below the observed edge limit. A transport failure leaves the run or
promotion unchanged: disconnect cancels only its observation, and the heartbeat
writer is joined before the handler returns. An individual stream write/flush
is bounded to five seconds, within the existing overall write deadline.

### Secret-store release preflight

An admin uplink can call `secretstore_verify` instead of entering the server pod:

```json
{"release_tier":true,"repo":"github/oberthci/oberth","paths":["oberth/upstream/oberthci/oberth/signing"],"timeout":45}
```

Use the exact upstream/org/repo identity and declared paths for the release.
`release_tier` selects the configured pipeline trust; `tier` defaults to
`release` and accepts `ci` only with a per-repo identity. Omitting
`release_tier` checks the server identity; that mode supports `keys` and
`expect` (for example `["signing/cosign.key,cosign.password"]`). Empty `paths`
uses the server's configured verification defaults. At most 32 paths and
expectations are accepted; timeout is bounded to 120 seconds including
TokenRequest, login and reads. No caller-selected connection or credential
settings are accepted. Tokens and fetched values remain in memory and never
appear in output; only field names are exposed when requested.

The result contains `verified` and bounded `output`. Failure sets MCP
`isError: true`, including when diagnostics exceed 64 KiB. Verification does
not modify infrastructure, grants, roles, or policies.

`status` with a branch such as `main` reports the current Oberth Git HEAD's
latest exact-SHA run, including promotion or source-branch CI. Older runs
named `main` cannot mask the current commit's result. A current HEAD without
a run returns `no-runs`; use a commit SHA to inspect historical evidence.

`promotion_list` is read-only. Optional `repo`, `source_sha` (full lowercase
40/64-character object ID), and `status` (`pending`, `passed`, `failed`,
`interrupted`) filter before pagination. `limit` defaults to 50 and cannot exceed
200. Results are newest admission first, including pending promotions without
a run or result. Pass `next_before` as `before` with the same filters to recover
older records. Each page observes current status; a status may change between
pages. The list neither retries a promotion nor proves publication; use its
full ID with `promote_status` and verify the resulting published ref normally.

### Scanning and reading the issue queue

Use `issue_list({"state":"open"})` for the open queue. A page contains up to
50 metadata records; follow `next_before` as `before`, keeping the same filters,
until the cursor is absent. Filtering happens before pagination. Omitted
arguments preserve the all-issues default. `state` accepts `open`, `closed`, or
`all`; `kind` accepts `manual` or `ci`. The optional `repo` selects actual stored
repository association: global manual issues are excluded even if their titles
mention that component. `query` matches title, body or branch without regard
to case. Each page includes `total` for the current filters, independent of
`before`, and `counts`: open/closed retain the selected kind, while manual/ci
retain the selected state. Repository, query and uplink scope apply to every
count.

For the latest 5, 10, or 50 created issues, set `limit` accordingly (1..50).
Ordering is descending issue ID, not most recently updated. Listing does not
include bodies. `issue_get`, `issue_list`, and `/api/issues` show `upstream`
(empty for a installation-wide manual issue).

To repair a mis-scoped manual issue without changing its ID, body, appends, or
history, call `issue_update({"id":1094,"upstream":"github"})`. An admin may
use an empty `upstream` or `"global"` to make it installation-wide. Other
uplinks can only move visible issues to upstreams in their scope; CI issues
cannot move. The CLI can call the same tool with
`oberth mcp call issue_update '{"id":1094,"upstream":"github"}'`.

`issue_append`, `issue_update`, and `issue_close` return only an acknowledgment:
`{"id":1094,"state":"open","updated_at":"2026-10-30T00:00:00Z","occurrences":1,"appended_bytes":42}`.
`appended_bytes` is the UTF-8 byte length of the caller's append text, excluding
the dated separator; it is zero for update and close. Read content separately
with `issue_get` when needed.

Read selected bodies in one call: `issue_get_many({"ids":[787,743,426]})`.
The `results` array preserves that order. Each result contains `id` and either
`issue` or `error` (`not_found` or `response_limit`). Both `issue_get` and
`issue_get_many` preserve full records and unlimited bodies by default for
compatibility. To reduce context use, pass either or both of:

- `fields`: a nonempty list of unique public field names. For example,
  `issue_get_many({"ids":[787,743,426],"fields":["id","title","state","updated_at"]})`.
  Only selected fields appear in each issue, including selected zero values.
  Supported names: `id`, `lock`, `upstream`, `kind`, `title`, `body`, `state`,
  `repo_id`, `branch`, `occurrences`, `ci_origin`, `ci_work_id`, `created_at`,
  `updated_at`. Internal storage and sequence fields cannot be selected.
- `body_limit`: a nonnegative byte limit for the retained prefix of each body,
  including occurrence history embedded in the body. For example,
  `issue_get({"id":787,"body_limit":4096})`. A longer body ends with
  `\n[truncated N bytes]`, where N is the exact number of withheld source bytes.
  The prefix ends at a UTF-8 boundary; the marker is additional to the byte
  limit. Zero returns only the marker for a nonempty body, and an empty body
  stays empty. The numeric `occurrences` count always remains exact. This
  option has no effect when `fields` excludes `body`.

The bulk encoded structured response remains limited to 256 KiB, after field
selection and body truncation. Every requested ID has a result, and later small
issues can still fit after a large issue. Retry `response_limit` IDs with compact
options or `issue_get`. Requests must contain 1..50 unique positive IDs. Neither
read tool acquires or renews locks, and neither changes stored content. This is
live pagination, not a point-in-time snapshot.

For a compact run read, use `run_get({"id":"<run ID or unique 8+ character prefix>","summary":true})`.
It returns `id`, `repo` (repository name), `ref`, `sha`, `size`,
`declared_resources`, `queue_position` when available, `superseded`, `status`,
`phase`, `failed_burn`, `failed_step`, `error`, and `steps`. Each step contains
`burn`, `name`, `status`, `started_at`, and `finished_at`; unknown times are null
and an empty step list is `[]`. Burn names distinguish steps with the same name in different burns. This read omits
retained timing, the full repository record, diagnostics, and telemetry. It
does not load the retained timing record. Omit `summary` or pass false for
the existing full `Run`/`Steps`/`Repository` response. Authorization and upstream scope are
the same for compact and full reads.

### Server-side wait correlation

Authenticated `wait` and `promote_status` requests carry a server-generated
`X-Oberth-Wait-ID` response header. After selecting JSON or SSE transport, the
server emits one fixed-schema start event and one terminal event. Preparation
refusals retain JSON and use `preparation-refused`; their diagnostic duration
starts after preparation. Events contain only UTC, nonce, fixed tool and mode,
elapsed milliseconds, bounded frame counts and a closed outcome. Arguments,
RPC IDs, identities, targets, headers, response bodies and raw errors are absent.
Unknown internal errors retain their generic public response, using the same
nonce without the older raw-error log for these two tools.

A `final-frame-flushed` outcome means the server completed its write/flush. It
is not acknowledgement by the client or proxy. Disconnect, write/deadline,
marshal and panic outcomes retain the observer cancellation and heartbeat-join
contract. No automatic retries or durable-operation cancellation are added.
The nonce is not authority; a client must actually expose the response header
before a caller can claim client/server correlation. These diagnostics do not
establish the cause of historical uncorrelated 502 or QUIC failures.

## CLI long-poll timeouts

`oberth mcp call` derives its client deadline from the tool's `timeout` argument
plus 30 seconds, with a minimum of 60 seconds. `wait` and `promote_status`
default to a 600-second tool timeout when omitted or zero, so their default
client deadline is 630 seconds. The server rejects tool timeouts above 600
seconds.

```bash
oberth mcp call --token-file ~/.config/oberth/token wait \
  '{"sha":"<tested-sha>","timeout":600}'
oberth mcp call --token-file ~/.config/oberth/token --timeout 11m wait \
  '{"sha":"<tested-sha>","timeout":600}'
```

`--timeout` accepts a positive duration such as `90s` or `11m` and overrides
the client deadline, including for tools without a timeout argument. A shorter
override can end the observer before the server returns; the error names both
limits. Caller cancellation and earlier caller deadlines still take precedence.
Ending a long-poll request does not cancel the durable run or promotion.
Artifact downloads with `artifact_get --output` retain their streaming default;
an explicit `--timeout` bounds the download too.

Archive a retired repository with `oberth repo archive upstream/org/repo`;
restore it with `oberth repo unarchive upstream/org/repo`. Both use the remote
admin MCP tools and accept `--url`, `--token-file` and `--ca-file`. Archive refuses
in-flight work and pending promotions, keeps all evidence, drops the Git cache,
and refuses pushes and new runs until restored. History remains readable.
Remove its releaseWIF mapping and list it under chart `archivedRepositories` to
make stale release capabilities fail chart validation.


## Tool results and refusals

Every tool advertises an object `outputSchema` in `tools/list`. When present,
`structuredContent` is always a JSON object. List results use a named array:
`promote_grant_list` returns `{"grants":[]}` when no grants match, just as
`run_list` uses `runs` and `repo_list` uses `repositories`. Successful
`issue_delete` returns `{"id":123,"deleted":true}`. Clients consuming the old
bare promotion-grant array or null deletion result must decode these objects.
The read-only HTTP `/api/grants` endpoint retains its existing array response.

Ordinary JSON tool results carry the same object in their text content. Log
and diagnostic tools retain their readable text view. `oberth mcp call` prints
that text, or the structured object when a peer supplies no text blocks.

A refused tool sets `isError:true` and returns text with the stable error class
followed by an explicitly safe reason, for example:

```text
forbidden: promote requires an admin uplink; mint one with: oberth uplink add --admin - <identity>@<host> < key.pub
forbidden: no matching promotion grant; request one from an admin with promote_grant
service unavailable: promotion grants
```

Only public policy/capability reasons are included. Dependency error chains
remain masked. A resource outside the uplink's scope still answers like a
missing resource, without revealing its existence. An HTTP authentication
failure is separate from an authenticated tool refusal.

### Queue priority administration

The four execution classes are `low < standard < promotion < high`. Ordinary
builds default to standard. Promotion CI and eligible oldest tag/releases use
promotion; admin RC globs also select promotion. After 60 minutes standard work
joins promotion at its original FIFO position. No automatic path creates high.
High requires explicit unscoped-admin or unexpired high-grant authority. A
promotion-only grant cannot authorize high. Already admitted/running work is
never preempted, and all pressure/resource/group/hold/publication guards remain.

`run_priority` sets a waiting run to any of the four names, with a full ID or
unique 8+ hexadecimal prefix and a 1–512 byte reason. It supports explicit
lowering and pins the class against aging. Unscoped admin only; actor, previous
class, selected class and reason are audited atomically. Restart and automatic
refire preserve the override, reason and admission place. Manual retries receive
a new place. Local updates refresh waiting reservations; external updates poll
within 15 seconds without blocking on SQLite contention.

`queue_priority_config {"rc_patterns":["rc/*","release/*"]}` replaces the
installation's at-most-16 case-sensitive globs; `*`/`?` match slash, `[]` disables.
`queue_priority_get` returns globs and `starvation_minutes:60`. Patterns grant
promotion classification only. Pipeline declarations cannot set queue urgency.

Optional `priority`/`priority_reason` appear on promote, rerun, retry, sync and
schedule_trigger, and in authenticated push options such as
`-o oberth.priority=high`. Low/standard requests need no urgency grant;
promotion/high require an unscoped admin or an unexpired `priority_grant`
(`max_class: promotion|high`). Push checks the exact key, not an identity alias.
Repository schedule defaults use the same names; automatic cron requests as
`oberth:scheduler` and still needs its own grant. Invalid/refused requests retain
the natural class and explain `priority_refusal`. Sync only raises its existing
waiting build; lowering requires run_priority. Accepted nonstandard priority and
automatic protection survive ordinary supersession; explicit replacement still
requires requester/admin authority and all existing branch/publication checks.

Tag automatic priority remains conditional on oldest waiting tag, size maximum
and size_revoke. Losing that boost retains an admin override or accepted request,
otherwise standard. Release execution, measured backfill, head reservations,
strict FIFO, bounded liveness escape and pressure constraints are unchanged.

Run fields `priority_class`, `priority_reason`, `priority_requested_by`,
`priority_refusal` and queue position report the effective class and provenance.
The existing detail badge uses these four canonical names. List/history rows
continue using only the server's bounded display_reason; no new status or layout
is added. Standard has no priority prefix; explicit nonstandard classes and aging
use the existing reason line. Automatic promotion/tag urgency is implied by ref.

Deprecated input normal maps to standard; explicit authorized high/highest and
legacy grants map to high. MCP enums retain the normal/highest input aliases for
current clients, while responses use canonical names. Automatic historical rows
with requested='' remain protection evidence, never high authorization.
The new timestamp migration preserves raw stored priority/audit evidence and all
shipped migration semantics. See [execution priority](release-priority.md) for
upgrade mapping, rollback and detailed authority/fairness rules.

## Additive API names and conditional issue updates

See [HTTP/MCP compatibility](api-compatibility.md) for preferred snake_case
fields, run ID aliases, opt-in collection envelopes and the deprecation policy.
Existing names and default envelopes remain supported throughout v0.17.x,
with the documented `/api/issues` privacy exception.
Issue metadata writes can pass `expected_revision` from a prior read; mismatch
is a conflict, while omission preserves last-write-wins. See
[conditional metadata updates](issue-lifecycle.md).

The current MCP surface has **80 tools**, including administrator-only
`publication_abandon`.

The CLI uses `--url`, `OBERTH_MCP_URL`, or `OBERTH_BASE_URL` (with
`/mcp` appended). `OBERTH_TOKEN_COMMAND` and
`OBERTH_CA_CERT` follow the installer environment; explicit flags take
precedence. With no configured endpoint, the CLI refuses before reading a
credential file, running a token command or creating a transport. There is no
hosted fallback: load the installer-provided environment or set the URL
explicitly. This credential-safety change also applies to older scripts.
MCP and REST both refuse plain HTTP except to
`localhost`, `127.0.0.1`, or `::1`, before resolving credentials. Redirects
cannot downgrade HTTPS to HTTP.

## Identifier and repository selectors

Run and promotion ID arguments accept a full hexadecimal ID or a unique prefix
of at least 8 characters. Ambiguous prefixes list the matching visible full IDs;
use a longer prefix or the full ID. This also applies to ID pagination cursors,
`run_priority`, `admission_release`, `promote_status`, and `promote_retry`.
Issue IDs and grant IDs are decimal integers and must be supplied in full.
Publication IDs (for `publication_abandon`) remain exact IDs.

Repository arguments accept the fully qualified `upstream/org/name` form, for
example `codeberg/acme/widget`. A bare `widget` or `acme/widget` is accepted only
when unique among repositories visible to the caller. Ambiguity errors list
all visible fully qualified candidates; pass one as `repo` to `promote`,
`status`, `sync`, `wait`, or other repository-taking tools. The same form works
with CLI `--repo` options and MCP JSON arguments. When an omitted repository
makes a SHA or branch ambiguous, the error lists candidates as well.

### YAML concurrency groups

`concurrency_groups({})` and `oberth admission groups [--json]` list definitions,
limits, holders and waiters. They are read-only and require an unscoped uplink;
an admin role is not required. `system_status.admission.groups` provides the
same data. Configure only `oberth.ci/concurrency-group` and
`oberth.ci/concurrency-limit` in the pipeline YAML; the default limit is 1. See
[concurrency groups](concurrency-groups.md).
