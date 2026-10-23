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
Dependency checks run immediately in the background and then every two seconds,
with a three-second probe deadline. `/readyz` reads the snapshot without database,
credential or Kubernetes calls. Missing, failed or older-than-15-second samples
fail closed. Upstream forge reachability remains a diagnostic rather than a
readiness dependency.

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

For an optional quiet-queue check, use `oberth upgrade --wait-idle --timeout=5m`.
It runs `oberth wait-idle` inside the selected context/namespace's Deployment,
reading the live daemon's complete queue through its private admin socket. It
does not depend on remote API credentials or a scoped repository view. A busy
queue is polled once per second until empty or the timeout expires; unavailable
state aborts the upgrade. Dry-run describes the check without waiting. This is
an advisory observation, not a freeze on incoming pushes, and requires a server
version supporting `wait-idle`.
