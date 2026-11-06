# Unreleased: v0.17.95

## Execution retirement (#1294)

**Breaking MCP change:** the registry now exposes 75 tools instead of 80.
`run_kvm_preflight`, `kubevirt_grant`, `kubevirt_revoke`, `kubevirt_grant_get`,
and `kubevirt_grant_list` are removed, along with `oberth kubevirt`, the retired
execution flags and the retirement census command. Generic
`workflow_capability_grant`, `workflow_capability_get` and
`workflow_capability_revoke` remain.

Oberth runs and gates Argo Workflows. VM admission/budgets, device injection,
trusted preflight, the protected VM executor, guest harnesses, image publishing,
launcher diagnostics and execution-journal APIs are removed. Pipelines own
resource creation, their harnesses, cleanup and gates through operator-owned
workflow capabilities. Ordinary workflows, resource admission, release/WIF
validation, nonroot leaves and branch placement remain.

Legacy Helm settings are accepted but ignored, with upgrade warnings. Legacy
workflow declarations fail with a message naming the workflow capability grant.
Old runs still list and render. Active retired executions fail clearly at
restart; external resources need operator cleanup. Drain them before upgrading.
The upgrade resource inventory needs cluster-wide list permission on KubeVirt resources.
Existing migration SQL is unchanged and no migration is added. Retired tables
are inert until a later removal migration; restore the pre-upgrade database for
rollback. See [migration instructions](../workflow-capabilities.md#migration-from-retired-execution).

## Log windows and response compression (#1486 follow-up)

JSON API and MCP responses of at least 1 KiB support gzip when requested by the
client. SSE and raw downloads keep their streaming behavior.

MCP `run_logs` and CLI `oberth log` now show the last 200 lines when no pattern,
limit, tail, offset or byte offset is supplied. JSON includes a `window`
explanation; MCP text and the CLI stderr summary explain it too. Explicit
filters retain their behavior. Use `--tail --limit 1000` for a larger CLI tail
or `--limit 0` for the existing bounded full read. MCP uses `tail: true` and
`limit: 1000`, or `limit: 0`, respectively.

## Queue demand and layout (#1487)

Queued and waiting runs show their peak concurrent pod requests, for example
`needs 9.9 CPU, 16 GiB · host CPU busy`. The line gives one blocker: CPU or IO
pressure, the running-run limit, another visible run, or queue position.
The figures use workflow requests and concurrency, not limits or retired
weighted budgets. Scoped viewers see their own figures; shared counts and
hidden repositories remain private. Unavailable figures are omitted.

Demand lookups share one two-second budget for the whole list. A bounded LRU
keeps successful results by run and SHA without expiry and caches failures
and timeouts for 30 seconds. Queued detail messages use plain wording too.

The priority prefix moved off queued lines; a badge shows low priority, high
priority or priority promotion. Standard runs have no priority label.
The dashboard queued block runs from last-to-start at the top to next-in-line
at the bottom, directly above running rows. The queued filter uses the same
order. Running and finished rows keep their existing order; API/MCP lists
continue to use claim order.
Priority remains in its fields and park-policy fallback details remain in the
run's durable reason and audit, without crowding the queued line. Kubernetes
resource waits retain `pod pending: insufficient cpu (4 CPU requested)`.

## Queued branch supersession (#1488)

A new branch-head push now supersedes older queued, not-started runs in the same
repository/ref/schedule lane at every priority, including runs an admin lowered
to low or standard. The replacement keeps the most recent explicit admin
priority, actor and reason, with a fresh queue position. Running runs keep their
existing supersession rules. Tags, promotions and pending publications retain
their existing protection.

The push confirmation counts superseded queued runs even when no Workflow needs
cleanup. Replayed receives retain that count without repeating the enqueue.

## Faster OpenBao builds (#1489)

The OpenBao integration step reuses the repository Go module and build caches,
with shared per-run cache mounts as a fallback. Setup tools remain read-only.
The installer integration binary no longer includes race instrumentation; the
unit suites retain it. OpenBao itself is still a pinned release download checked
against its SHA-256, and all policy-isolation and setup/TLS tests still run.

Local Linux arm64 measurements with Go 1.26.10 took 97.6 seconds with an empty
build cache and 3.82–3.93 seconds on three warm builds. Modules were already
cached. The exact warm step, including download, verification and real OpenBao
tests, passed in 41.0 seconds; the full cold-build-cache step passed in 133.1
seconds. These are local measurements, not CI-under-load results.

The release retains its existing 600-second compile budget, 791-second total
deadline and 120-second test window. Memory requests/limits remain 3200/4096 MiB,
and storage requests/limits remain 8 GiB. These release-base bounds are retained
while enabling the cache changes; remeasure on CI, including Argo helpers,
before reducing them.
