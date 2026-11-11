# Reading Oberth from a terminal

Every other `oberth` command opens the server's database and git cache directly,
so it only works inside the pod. These read the HTTP API, so they work from the
machine you pushed from.

## Configuration

`oberth install` offers to write this on an interactive install, right after it
registers the uplink, into `~/.config/oberth/env`. Source it, or copy the three
variables into your shell profile:

```
. ~/.config/oberth/env
```

Sourcing it is optional: every remote command, the REST reads (`runs`,
`run`, `log`, `repos`, `issues`, `status`, `artifacts`, `run resources`) and
the MCP-backed ones (`mcp call`, `run wait`, `events`, `run hold`, `admission`
and the rest) resolves the server the same way, one setting at a time:

- Server: `--url`; `OBERTH_MCP_URL` / `OBERTH_BASE_URL`; `OBERTH_BASE_URL`
  in `~/.config/oberth/env`; the url in `~/.config/oberth/mcp.json`.
- Token: `--token-file` (mode 0600); `OBERTH_TOKEN`; `OBERTH_TOKEN_COMMAND`;
  `OBERTH_TOKEN_COMMAND` in the env file; `~/.config/oberth/token` (mode 0600).
- CA: `--ca-file`; `OBERTH_CA_CERT`; `OBERTH_CA_CERT` in the env file;
  `~/.config/oberth/ca.crt`.

The token and CA from the files belong to the server the files name: when
`--url` or a variable names a different address, the files' token, token
command and CA are not used, and that address needs its own `--token-file`,
`OBERTH_TOKEN` or `OBERTH_TOKEN_COMMAND`. A token file with no server named in
`env` or `mcp.json` is not used either. The env file's `OBERTH_TOKEN_COMMAND`
runs only when the file is yours and not writable by group or others, and
none of the files (or the directory) is read when someone else can change
them.

`--url` takes the server's base address or its `/mcp` endpoint; either
command family derives the other form. `XDG_CONFIG_HOME` replaces
`~/.config`. With none of them set, a command says so and names the three
ways.

The rest of this section is what that file contains and why.

| Variable | Meaning |
|---|---|
| `OBERTH_BASE_URL` | The server's address. Setting it is what makes a command read the server |
| `OBERTH_TOKEN` | An uplink bearer token |
| `OBERTH_TOKEN_COMMAND` | A command whose standard output is the token, used when `OBERTH_TOKEN` is unset |
| `OBERTH_CA_CERT` | Inline PEM or path to a PEM file containing the authority that signed the server's certificate |

A token of an upstream-scoped uplink reads only its upstreams' runs, issues
and repositories; see [Limit an uplink to some upstreams](mcp-setup.md#limit-an-uplink-to-some-upstreams).

Oberth never writes a token to disk. There is no `oberth login`. `OBERTH_TOKEN_COMMAND` exists so the token can come from a
password manager or the system keychain without ever landing in a file:

```
export OBERTH_TOKEN_COMMAND='op read "op://vault/oberth/credential"'
```

The one exception is an explicit choice. On a host with no keychain,
`secret-tool` or `pass`, the client files `oberth install` writes read the
token from `~/.config/oberth/token`, and the credentials screen of
`oberth setup` offers **Save to file**: pressing it writes the bearer token
to exactly that file (mode 0600; a different token already there is kept as
`token.bak-<time>`). Nothing writes it unless you press it.

## Commands

```
oberth runs [--repo NAME] [--ref REF] [--limit N] [--before CURSOR] [--all] [--json]
oberth run <run-id>
oberth run wait <run-id> [--until finished|held|any] [--timeout SECONDS] [--json]
oberth events [--since ID] [--wait] [--timeout SECONDS] [--kind K]... [--repo R]... [--run ID]... [--own] [--json]
oberth sync status [--lock KEY] [--since 24h] [--json]
oberth run retry <run-id> [--json]
oberth run rerun <run-id> [--reason TEXT] [--force] [--json]
oberth run cancel <run-id> [--reason TEXT] [--force] [--json]
oberth promotion create --repo <repo> --branch main --sources-file sources.json [--skip-conflicting] [--json]
oberth promotion retry <promotion-id> [--json]
oberth runs cancel --repo NAME --ref BRANCH [--reason TEXT] [--force] [--json]
oberth admission [--json]
oberth admission release <run-id> [--force] [--json]
oberth log <run-id> --burn B --step S [--pattern P] [--context N] [--offset N] [--limit N] [--tail] [--byte-offset N]
oberth artifacts <run-id> [name]
oberth repos
oberth issues [--state open|closed|all] [--work-status S] [--limit N] [--before ID] [--all] [--json]
oberth status
```

The list and detail commands are read-only. `oberth run retry` calls the
`run_retry` MCP tool using the same uplink token; it queues a fresh attempt
for an interrupted or infrastructure-failed run and records the caller as actor.
Workflow-level failures without a failed step are eligible. The source must still
be the newest run for its ref, and the ref must have no active run. An admin can
also retry tag/release infrastructure failures before credentialed/publication
work started; the tag object, tested commit and release admission are retained.
Test failures, older runs and promotion/plan/apply runs cannot be retried this way.

`oberth issues` prints one page (at most 50, `--limit N`) and its full matching
count, such as `1–50 of 93`, followed by `more: oberth issues --before <id> (or
--all)` when older issues match. `--before <id>` continues from that cursor and
`--all` follows every page. `--json` prints the server's page envelope with its
`next_before` cursor; with `--all` it prints every row and `next_before: 0`. The
dashboard shows the same page range and state/kind counts.

`oberth run rerun` calls the `run_rerun` MCP tool with the same uplink
token. It reruns a failed or interrupted run at the same SHA and ref without
a push, for the cases `run retry` refuses: a failed test step, a run that is
no longer the newest of its branch (as long as the branch head is still that
SHA), and tag/release runs. The new run is linked to the original (`oberth
run` prints `rerun of`), and `status`, `wait` and `promote` on the SHA follow
it. A non-admin uplink may rerun branch-push runs; a tag/release rerun needs
an admin. Force-free tag replay requires the newest run, an unchanged exact tag
object, and safe evidence for every run of that tag. Submitted attempts need a
version 2 credential-aware plan and a durable complete terminal Workflow
observation. Secret-store and release WIF steps both count as credentialed.
A started credentialed step, incomplete observation (including observation
outages or synthesized interruption), legacy plan, missing evidence, older
source or moved tag requires `--force`. A missing journal is unknown unless
an observed terminal Workflow explicitly had zero nodes. Pre-intent attempts
with no step evidence can be replayed without a plan. These rules also apply
to tag infrastructure retries; `run retry` has no force override.
Pending Workflow deletion blocks retry and rerun even with force, until cleanup
is confirmed. A published/green tag remains refused. When every step passed and
only the upstream push failed, the push is retried instead of executing the
pipeline. Audit records include the replay basis and evidence counts.

`oberth promotion retry` calls the `promote_retry` MCP tool with the same
uplink token. A failed or interrupted promotion (the push failed, the target
moved, its CI hit an infrastructure failure, or a restart interrupted it) is
re-run as a new promotion of the same repository, source SHA and target:
merged onto the target's current head and re-tested, so nothing is ever
pushed that was not tested against the head it lands on. It needs the same
authority as `promote`; for a non-admin uplink the original promotion grant
still counts, because a grant is consumed only when its promotion is
delivered. Follow the new ID with `promote_status`. When delivery finds the
target moved, the server already retries on its own (at most twice in a
row), and `promote_status` on the failed promotion names the retry in
`retried_as`.
`oberth runs`, `oberth runs --active` and `oberth run` use one pre-start
label, `queued`, for every run that has not started (durable queued, or
claimed before admission), with the server's `display_reason` naming what it
waits for: `next in line`, `waiting for CPU budget (12.5 of 40 CPU free)`,
`host IO pressure (42%)`, `waiting for admission`; a submitted run the Argo
controller postponed reads `waiting for an Argo workflow slot (controller
parallelism)`. Admission records the start, so an admitted run is
`running` from the grant on. The default list puts the whole queue first, with the
run admitted next at its top (effective priority, then FIFO), then running runs newest start first, then
terminal runs newest finish first. A trusted
preflight is `running`. A replaced interrupted run displays `superseded`;
explicit cancellation and interruption without a successor display `cancelled`.
The reason identifies the successor, actor or retry action. `--json` keeps the underlying `Status` field and adds
`display_status` and `display_reason`. Successful releases display `passed`;
`release` is a separate kind. The `status`/`wait` MCP tool's legacy
`status: "delivered"` remains an alias of `passed`, alongside
`display_status: "passed"`.

The limit counts terminal rows; queued and running rows are additional on page 1.
`--all` follows the server cursor through terminal pages. `oberth runs --json`
prints `{"runs": [...], "next_before": N}`, the envelope of `GET
/api/runs` and MCP `run_list`: pass `next_before` to `--before` for
the next page (a scoped uplink gets `next_cursor`, a run ID, instead). After the
oldest run neither field is present. `--all --json` prints every page's rows in
one envelope. If a cursor run was
pruned, restart from the first page. Queue positions are global even when a
repository or ref filter is used, and withheld for scoped uplinks. The deprecated
`oberth runs --active` compatibility view stays unpaged and uses the same server
queue positions. Elapsed time begins at the admission grant, includes preflight,
and resets at re-admission after a VM-capacity park. Interrupted results neither
open nor resolve CI-red issues.

`oberth run cancel` calls the `run_cancel` MCP tool with the same uplink
token. It cancels one queued, waiting or running run: the run becomes
`interrupted` with phase `cancelled` (shown as `cancelled` by `oberth runs`
and `oberth run`) and a reason naming the caller; a claimed run's Workflow is
deleted and its admission capacity released immediately; it is never re-fired
and opens no CI issue. A non-admin uplink may cancel only branch-push runs
in repositories it can see. An admin may cancel
any run, but a tag/release or apply run needs `--force`, because anything its
Workflow already published stays published. Cancelling promotion CI fails the
promotion without merging. A terminal run is refused. `oberth runs cancel`
cancels every active run of one ref the same way, reporting each refusal.

`oberth admission` prints the server's admission gate from `system_status`:
how many runs are admitted (Argo controller `parallelism` bounds how many
Workflows run; Oberth keeps no run ceiling), an active administrator hold with
its whole reason, host pressure, the queue-head run every later run waits
behind, and every reservation with its state (`preparing`, `waiting`,
`granted`, `retained`), repository/ref and age. There are no sizes, weights or
concurrency groups: shared step limits are native Argo `synchronization`
([where limits live](where-limits-live.md)). `oberth admission release <run-id>` calls the admin-only
`admission_release` MCP tool to free a reservation no owner will release,
typically a granted one still held for a run that is already terminal. It
deletes the run's Workflow first as proof; `--force` releases without that
proof (a failed deletion, or a run in a trusted preflight that has no recorded
Workflow), and the audit records it unverified. A live run's reservation is
refused: cancel the run instead. The gate lives in the server process, so both
are remote-only.

Inside the server pod, pass `--database /data/oberth.sqlite` instead of the
MCP flags: the command then passes the live daemon's audit mutation gate,
records the same audited cancel as the administrative actor, and wakes the
daemon's cancellation pass over its private admin socket.
Publishing stays in the dashboard and MCP.

`--json` on any of them emits the server's own payload unchanged, so a script
never parses a rendered table.

`oberth log` accepts the same filters as the `logs` MCP tool and prints the
same counts, on stderr. A step log can exceed a context window, so prefer a
pattern over retrieving the whole thing:

```
oberth log run-abc --burn ci --step test --pattern 'FAIL|Error' --context 3
```

Log reads have bounded work: at most 4 MiB of retained-file discovery for an
active run, then at most 4 MiB of filtering (including tail lookback retries).
Head reads stop when the returned-line limit is met; regexes never scan the
whole step solely to count matches. At most two interactive API/MCP run-log
pattern scans execute at once across the process, with at most one per
authenticated bearer token across MCP and dashboard reads. Token admission comes
before global admission, so one token cannot reserve both slots. Queued
requests use their existing ten-second read deadline; cancellation releases
queued admission state. Distinct tokens sharing a display identity remain
independent. Plain log reads and live-log polling do not use pattern slots. Internal CI issue excerpts keep their byte/record bounds
and do not wait for those interactive slots.

Running runs retain an in-memory range index per run. Each request extends it
by at most 2 MiB of new bytes, without rediscovering bytes already in that index.
Repeated head requests can therefore find steps beyond the first 4 MiB even
without changing the cursor. Until the index catches up, deep cursors and tail
reads use a bounded discovery window within the same 4 MiB discovery budget.
Use `has_more` and byte cursors for complete traversal, including empty pages.
The cache keeps at most 64 runs (including detached readers), evicts idle entries
by least recent use, and stores each distinct step label once. Each run can
retain 64K compact ranges, up to 4096 distinct labels / 128 KiB of label bytes,
and a 64 KiB partial-record prefix. A shared 512K-slot range capacity budget
covers all runs, including in-flight entries being forgotten. When capacity or
label bounds are reached, the index drops older ranges and continues indexing
the newest output. Byte cursors into discarded coverage use bounded raw-window
discovery; completeness/count flags remain conservative. Default head polling
can still need byte continuation once its old prefix has been discarded.
Finishing or purging a run releases its entry; restarting starts the cache cold.
On observed growth, a one-byte newline check at the last complete record catches
some same-inode truncate/regrow cases; its byte is included in `discovery_bytes`
in addition to new bytes. Production logs are append-only: this guard does not
detect every rewrite that preserves the boundary newline.
No active read publishes the durable terminal index.

Individual records above 64 KiB are omitted and counted as `omitted_long_lines`. Output remains capped at 4 MiB.

Structured HTTP/MCP responses report `scan_complete`, `bytes_exact`,
`scanned_bytes`, `discovery_bytes`, `total_scanned_bytes`, `scan_limit_bytes`,
`withheld_lines`, `withheld_bytes`,
`has_more` and `next_byte_offset`. Line/match/withheld-line counts are lower
bounds unless `scan_complete` is true; byte/withheld-byte counts are lower
bounds unless `bytes_exact` is true. Counts describe this call, not cumulative
paging totals. `total_scanned_bytes` sums raw-log discovery and filtering,
including buffered read-ahead and repeated tail lookbacks; existing per-phase
counters keep their meaning. `scan_limit_bytes` reports the hard aggregate bound:
4 MiB for terminal logs and 8 MiB for active logs (at most 4 MiB per phase).
Index metadata has its separate bounded reader and is not raw-log scan bytes.
The CLI prints the consumed scan budget in its summary; JSON carries the same
fields. A zero-match incomplete page does not prove absence.

When `has_more` is true, pass `next_byte_offset` as `byte_offset` (CLI:
`--byte-offset`), keep the same `tail`, `pattern` and `context`, and reset
`offset` to zero. The cursor is an absolute retained run-file byte position;
head pages move forward and tail pages backward. `offset` still skips lines or
matches inside the scanned window, including with tail. This differs from a
global offset from the step start: tail lookback can move that window during
the call. Use byte cursors with `offset=0` for stable continuation. Context is
local to each page; it can be clipped at page boundaries. Logs can grow while active, so these are not
snapshot cursors. `oberth log` and `oberth mcp call ... run_logs` print the
completeness/count/cursor summary on stderr. MCP text appends a continuation
trailer whenever `has_more` is true; an empty incomplete page explicitly tells
the client to continue. `structuredContent.output` remains the exact raw slice
without that trailer. Completed text pages keep the raw output unchanged.

Run/log tool reads have a 10s server budget including scope and DB-pool waits;
optional live scheduling observation has its existing 2s sub-budget. Bearer
authentication has a separate 5s budget for fresh identity/scopes/revocation.
A due `last_used_at` update is best-effort telemetry, queued to one background
background task with at most 64 queued distinct tokens plus one in flight per Store.
Pending IDs are deduplicated, so repeated use of one token cannot fill the queue.
The background task records the accepted request's use time, not the delayed commit time,
and does only an UPDATE; it never consumes authentication slots or readers.
Authentication does not wait for telemetry's writer slot or commit. A full queue,
failed update or shutdown can drop telemetry. A failed update suppresses all
telemetry attempts for one minute; queued and incoming uses during that cooldown
are dropped. Later requests retry while the committed timestamp remains due.
A committed timestamp newer than one minute skips enqueueing. Server
token lookups and SSH share four reserved read-only auth connections, separate
from the eight ordinary dashboard/list readers. Per-Store admission limits each
protocol to three reads, including connection waiters, preserving at least one
connection for the other protocol. Waiting uses the caller's existing deadline.
Malformed bearer strings are rejected before hashing or database access: issued
tokens are `oberth_` plus 43 unpadded URL-base64 characters (50 bytes total).
Correctly shaped unknown tokens still require a fresh lookup. Due updates are
conditional so concurrent requests write the timestamp at most once per minute.
The administrator metrics endpoint exposes `oberth_token_use_attempts_total`,
`oberth_token_use_failures_total`, `oberth_token_use_queue_drops_total` and
`oberth_token_use_cooldown_drops_total`, without token identifiers.
The background task is canceled and joined when the Store closes; a background task inside SQLite's
external busy wait may still take up to the busy timeout to return. Failed
authority reads, including non-context database errors, fail closed with
retryable HTTP 503. Only invalid credentials receive an invalid-token HTTP 401.
CPU scheduling or a stuck kernel file read can still delay wall-clock return;
cancellation is checked between bounded reads. `run_get` does not fetch pod logs and returns the summary by default; pass
`summary:false` only when timing and repository details are needed. This does not change artifact reads or
live log streaming.

The `[burn/step] ` prefix is stripped from each line, since the step is already
named in the command. Argo executor lifecycle lines (`time=… level=INFO …
argo=true`, such as "waiting for signals" or "sub-process exited"
error=<nil>) are hidden and counted on stderr; a warning, an error or a
non-nil exit status is always shown. `--raw` keeps the prefix and every
executor line, and `--json` returns the server's payload unchanged. The
dashboard folds the same lines behind a "▸ N Argo executor lines" toggle.

## Push options

`git push -o <option>` passes these to Oberth; each push may carry up to 32:

| Option | Effect |
| --- | --- |
| `oberth.priority=<low\|standard\|promotion\|high>` | Requests an execution class; see [release priority](release-priority.md). |
| `oberth.priority_reason=<text>` | Explains the priority request (1..512 bytes, no control characters). |
| `oberth.supersede=true` | Lets this push supersede protected (prioritised) work on the branch; replacing it requires its requester or an unscoped admin. See [release priority](release-priority.md). |
| `oberth.assist=1` (or `true`) | Asks for agent-assisted retry holds; see [assisted continuation](assisted-continuation.md). |

An unknown `oberth.*` option, or a known one with an unrecognised value, never
refuses the push: it is ignored and the push prints one line such as
`remote: oberth: ignored unknown push option oberth.priorty (known: ...)`.
Options outside the `oberth.` namespace are left alone.

If creating the run fails after Git accepted the refs, the push exits non-zero
with `oberth: refs accepted, but creating the run for <ref> failed: <cause>`.
The record stays in the receive journal and is delivered again by the next push
to that repository or a server restart. While it is stuck, later pushes to the
repository are refused before any ref moves, naming the record. An unscoped
admin inspects it with `oberth outbox show <upstream/org/repo>` and retries it
with `oberth outbox retry <upstream/org/repo>` (or abandons it with `--id` and
`--reason`).

## Local or remote

`artifacts` reads either. **If `OBERTH_BASE_URL` is set it reads the server;
otherwise it reads the local store**, which is the only thing that works inside
the pod. Every command that can go either way prints which it used:

```
reading: server
reading: local store
```

That line goes to stderr, so it never contaminates output you pipe or redirect.

## Certificates

TLS verification is always on and there is no flag to disable it. A CI client
that can be told to trust anything is a CI client that will be.

Two failures look similar and need different fixes, so the client names which
one happened.

**Unknown authority** means the signer is not in this machine's trust store.
Set `OBERTH_CA_CERT` to the PEM certificate itself (containing `-----BEGIN`)
or the path to its PEM file. OpenSSL preambles and a BOM are accepted. `--ca-file` overrides this value for MCP commands.

**A hostname mismatch** means the certificate does not cover the address you
used, and no trust anchor can fix that. The client lists the names the
certificate does carry:

```
client: the server's certificate does not cover "localhost"; it is issued for
oberth, oberth.oberth, oberth.oberth.svc, oberth.oberth.svc.cluster.local
```

The chart issues a certificate for the four in-cluster names, plus whatever the
deployment adds with `tls.extraDNSNames` and `tls.extraIPs`
(`oberth install --tls-extra-dns-name`, `--tls-extra-ip`). A macOS kind install
names `localhost` and `127.0.0.1` by itself, since its NodePorts are bound to
the loopback interface and both are therefore true of it.

So a developer who hits this is looking at a deployment that has not named the
address they are using. Either reach it by a name the certificate carries, or
have whoever runs the server add the one they expect people to use. This is a
deployment decision, not something the client should route around, and it is
why the names are a chart value rather than a client flag.

Changing those values on an existing release has no effect on its own: the TLS
Secret is generated once and kept, so the certificate is re-issued only after
that Secret is deleted. Re-issuing does not invalidate an uplink, which is keyed
by SSH public-key fingerprint, but it does change the TLS fingerprint people
compared out of band.

For ordered multi-source promotion, see [promotion trains](promotion-trains.md),
including a three-source CLI, MCP and REST example. The single-source equivalent
is `oberth promotion create --repo <repo> --branch main --sha <full-sha>`.

## Assisted continuation

`oberth run rerun <id> --assist` asks for retry holds; `oberth run hold
list|exec|patch|continue|abandon <id>` drives a held step (MCP `run_hold_*`,
`run_continue`). `run hold exec` takes the command after `--`; `run hold patch`
reads a unified diff from `--diff-file <path>` or `-`. See
[assisted continuation](assisted-continuation.md).
