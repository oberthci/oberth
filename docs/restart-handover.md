# Restart handover

An ordinary `oberth upgrade` can proceed with a busy queue. Adoption preserves
eligible Argo Workflows under their existing run IDs; it does not create a second
Workflow or repeat a publication. Running branch runs are adopted. Promotion,
tag and release runs are adopted where the server supports it (#1172 slice 2);
otherwise their Workflow is deleted and the run is recorded `interrupted`, as
before. Branch runs with a pending trusted KVM preflight are not adopted: KVM
preflights keep their separate cleanup-and-interrupt recovery until conductor
reattachment is available. Admission and audit checks still apply, and schema
rollback still requires the documented restore procedure.

Live Argo output is retained as each pod writes it. In-process reconnects use
an inclusive Kubernetes timestamp plus the ordered records at that timestamp,
so repeated identical lines remain distinct. Adoption currently has no durable
pod cursor: retained bytes are preserved, historical output is skipped, and the
step records a continuity-gap diagnostic. New output follows from the adoption
time. Previously terminal steps also report that their historical completeness
cannot be established. Browser polling and pod delivery have separate health
indicators; a quiet command can have a healthy connection without new output.

On SIGTERM the server marks `/readyz` unavailable before cancelling scheduler
workers. New HTTP/MCP requests receive 503 with `Retry-After: 1`; `/healthz` stays
healthy during drain. Scheduler observers stop and adoption-capable Workflows
continue in Kubernetes. Already accepted mutations retain their request contexts
and can finish. HTTP connections share a 15-second graceful drain budget, after
which remaining connections are closed. Scheduler observers then get up to 30
seconds to record terminal results, and API background work up to 10 seconds
to stop: 55 seconds in the worst case. The chart's
`terminationGracePeriodSeconds` (default 75, minimum 60) covers that before
Kubernetes sends SIGKILL; a pod killed early loses the clean handover, not the
adoptable Workflows.

Readiness stays unavailable until owner recovery and Workflow adoption finish.
Database/configuration checks run immediately in the background and then every
two seconds, with a three-second probe deadline. `/readyz` reads the snapshot without database,
credential or Kubernetes calls. Missing, failed or older-than-15-second samples
fail closed. Kubernetes availability, periodic audit health and upstream forge
reachability remain status diagnostics rather than readiness dependencies.
Startup audit verification, owner recovery and operation-level mutation gates
remain required; keeping the endpoint routable does not authorize writes.

`oberth mcp call` reconnects only for an explicit list of pure reads: `wait`,
`status`, `logs`, `system_status`, `run_get`, `run_logs`, `run_list`,
`run_kvm_preflight`, `artifacts`, `artifact_get`, `promote_status`,
`promotion_list`, `promote_grant_list`, `repo_list`, `issue_get`,
`issue_get_many`, `issue_list`, `access_list`, `kubevirt_grant_get`,
`kubevirt_grant_list`, `size_grant_get`, `uplink_list`, `audit_list` and
`schedule_list`, plus protocol initialization, ping and tool listing. A test
proves none of them is behind the server's audit mutation gate. Authenticated
JSON GETs used by CLI status/run views also reconnect. Retries preserve the
token, exact arguments and original client deadline, except that a `wait` or
`promote_status` retry shrinks its server-side `timeout` to the client deadline
left (less a margin of up to 30 seconds), so the server still answers "still
running" before the client gives up (#1118). Backoff starts at 100 ms and
doubles up to two seconds, with a 120-second retry window starting at the first
transient failure, enough for a Recreate rollout; `--reconnect-window` changes
it. Shorter caller deadlines take precedence. Connection refusal/reset, connect
(dial) timeouts, truncated transport reads and HTTP 502/503/504 are transient;
authentication, certificate, validation and ordinary tool errors are returned
immediately. Partial responses are never printed before a retry, and
streaming artifact downloads are not replayed after writing output.

Draining MCP `wait`/`promote_status` observers receive JSON-RPC error `-32001`
(`server restarting`) in their final JSON/SSE response. Other MCP clients can
reconnect those observations using the same SHA/run/promotion identity. An SSE
connection that ends before a result can also be retried for these reads.
Mutations such as `promote`, `sync`, `run_cancel` and `admission_release`, and
unknown tools, are never automatically retried, even after 503. If a mutation's
reply is lost, inspect its durable result before deciding what to do next.

For an optional quiet-queue check, use:

```sh
oberth upgrade --yes --wait-idle --wait-timeout=2h --timeout=5m
# Or wait only for critical work; ordinary branch/schedule CI may remain:
oberth upgrade --yes --wait-for=critical --wait-timeout=2h
```

`--wait-idle` waits for all queued and running runs. `--wait-for=all` is equivalent;
`--wait-for=critical` also enables waiting, but excludes ordinary branch/schedule
CI. Tags/releases, promotions, plans, applies and unknown trigger kinds remain
critical. This selection is a conservative operator preference, not a claim
that every critical Workflow is lost on restart: current servers adopt eligible
branch, promotion and tag/release Workflows as described above. Older servers,
missing Workflows, applies and pending trusted preflights retain their distinct
interruption/recovery rules.

The queue wait has its own positive `--wait-timeout`, default two hours. The
separate `--timeout` (default five minutes) applies to Helm/rollout and version
verification. An earlier caller deadline or SIGINT/SIGTERM cancels observation.
The local kubectl process is explicitly bounded by the queue-wait context;
joining inherited output pipes has a separate five-second cleanup bound.
Timeout, cancellation, unavailable queue state or a killed local child aborts
before any Helm upgrade or controller-profile mutation. The error identifies
local `kubectl exec`, elapsed time, the configured limit and that the upgrade
was not attempted. There is no automatic retry or assumption that an unreadable
queue is idle.

The command reads the selected context/namespace's live daemon through its
private admin socket, without opening the database from another process or
using a scoped API token. A single database observation counts the complete
nonterminal-run queue. Polling occurs once a second; queue size, branch/critical
counts, selection and elapsed time are printed immediately and every 15 seconds
while busy. Host progress is streamed as it arrives, with an elapsed heartbeat
even when an older in-pod binary cannot report counts. Critical selection needs
the new server's versioned queue snapshot and in-pod flag support; an older
server fails closed rather than silently treating unknown classes as branch CI.
An old in-pod binary that rejects `--wait-for` reports
`server too old for --wait-for=critical; use --wait-for=all`.
Default all-run waiting retains compatibility with older `wait-idle` servers.

Dry-run describes the selection and both timeouts without waiting or upgrading.
Refused or failed upgrades return nonzero. An already-installed target with no
values or controller-profile change requested exits zero and prints
`Already running vX; no upgrade performed`. Successful upgrades, help and
explicit dry-runs also return zero. To reapply values at the current version,
supply those values as usual.

This is an advisory observation, not a freeze on incoming pushes or a transaction
with Helm: new work can arrive between the empty observation and rollout. It
counts nonterminal runs, not every possible external operation. It requires a
server version supporting `wait-idle`.

When piping output, retain the upgrade's status:

```sh
set -o pipefail
oberth upgrade --yes --wait-idle --wait-timeout=2h 2>&1 | tail -10
```

Without `pipefail`, a pipeline ending in `tail` usually reports `tail`'s success
even when Oberth exited with an error. No process can handle SIGKILL or make
itself immune to an external process-group kill; use a caller/session lifecycle
that lasts for the chosen wait. Detaching a process is not a queue-safety gate.
