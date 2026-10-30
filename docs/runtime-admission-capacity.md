# Change admission capacity without restarting

`--max-concurrent-jobs` remains the boot default (1–64). Optional
`--max-concurrent-jobs-file` names a mounted text key with one integer in that
range. The server opens it again every five seconds, following Kubernetes'
ConfigMap symlink updates. The weight budget is the effective jobs value times
the configured M weight. Runtime changes require any positive XL weight to fit;
XL=0 continues to mean the whole budget. Existing boot configurations retain
their historical oversized-weight clamp and warning.

The chart enables `admission.runtimeCapacity.enabled` by default. It creates
`<release-fullname>-admission` with key `max-concurrent-jobs` and mounts its
whole directory read-only, without `subPath` or a rollout checksum. Initial data
comes from `maxConcurrentJobs`, unless
`admission.runtimeCapacity.maxConcurrentJobs` overrides it. An administrator can
edit this ConfigMap directly while the server is running; allow for Kubernetes'
projection delay plus the five-second poll. A later Helm reconciliation restores
the value rendered from chart values. Set `enabled: false` to use the boot flag
alone on the next rollout. No ConfigMap API permission is added to the server.

Every accepted value or source change is audited before application as
`admission.capacity.change`, including the previous/effective job values, source
and weight budget. A malformed, missing, unreadable or oversized key, an XL
that would not fit, or an unavailable audit writer leaves the last good value
in force. Before the first accepted override, that value is the boot default.
Errors warn in logs and appear in `system_status.admission.configuration` and
the dashboard's admission-capacity card; they never pause admission. Audit IO
runs outside the admission mutex with a five-second deadline. The queue keeps
using its current budget while an update is awaiting audit.

Lowering capacity never cancels or reprices a granted/retained run. Usage may
exceed the new budget until existing work releases its grants; new work waits
for room. Waiting weights, the grant/run ceiling and dispatch allowance use the
new limit. Raising capacity wakes the queue. A running whole-budget XL remains
exclusive even after an increase. Disk floor, IO pressure, priority, backfill,
concurrency groups and VM limits still apply. This is weighted capacity, not a
strict count of all sizes: S runs can pack more densely than M runs.

The effective value, boot default, source, file, last accepted change time and
last reload error are exposed with the aggregate admission status. Scoped
status continues to omit host-wide capacity and configuration. The file key is
limited to 64 bytes and its invalid contents are not reflected in diagnostics.
