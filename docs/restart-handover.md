# Restart handover

An ordinary `oberth upgrade` can proceed with a busy queue. Adoption preserves
eligible Argo Workflows under their existing run IDs; it does not create a second
Workflow or repeat a publication. Trusted KVM preflights still use their separate
recovery policy until conductor reattachment is available. Admission and audit
checks still apply, and schema rollback still requires the documented restore
procedure.

On SIGTERM the server marks `/readyz` unavailable before cancelling scheduler
workers. New HTTP/MCP requests receive 503 with `Retry-After: 1`; `/healthz` stays
healthy during drain. Scheduler observers stop and adoption-capable Workflows
continue in Kubernetes. Already accepted mutations retain their request contexts
and can finish. HTTP connections share a 15-second graceful drain budget, after
which remaining connections are closed.

Readiness stays unavailable until owner recovery and Workflow adoption finish.
Dependency checks run immediately in the background and then every two seconds,
with a three-second probe deadline. `/readyz` reads the snapshot without database,
credential or Kubernetes calls. Missing, failed or older-than-15-second samples
fail closed. Upstream forge reachability remains a diagnostic rather than a
readiness dependency.

`oberth mcp call` reconnects only for an explicit list of observations: `wait`,
`status`, `logs`, `run_get`, `run_logs`, `promote_status`, `promotion_list`,
`system_status` and `artifact_get`, plus protocol initialization, ping and tool
listing. Authenticated JSON GETs used by CLI status/run views also reconnect.
Retries preserve the token, exact arguments and original client deadline. Backoff
starts at 100 ms and doubles up to two seconds, with a 30-second retry window
starting at the first transient failure. Shorter caller deadlines take precedence.
Connection refusal/reset, truncated transport reads and HTTP 502/503/504 are
transient; authentication, certificate, validation and ordinary tool errors are
returned immediately. Partial responses are never printed before a retry, and
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
