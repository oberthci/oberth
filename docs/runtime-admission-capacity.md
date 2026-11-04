# Change admission capacity without restarting

`--max-concurrent-jobs` remains the boot default (1–64). Optional
`--max-concurrent-jobs-file` names a mounted text key with one integer in that
range. The server opens it again every five seconds, following Kubernetes'
ConfigMap symlink updates. The effective value caps granted workflows.
Kubernetes decides pod placement from declared requests. S/M/L/XL labels and
legacy weights do not constrain this ceiling; setting it to one is valid.

The chart enables `admission.runtimeCapacity.enabled` by default. It creates
`<release-fullname>-admission` with key `max-concurrent-jobs` and mounts its
whole directory read-only and optional, without `subPath` or a rollout checksum.
A missing ConfigMap cannot prevent the server Pod from starting; on that restart
the boot flag is the initial last-good value. Initial data
comes from `maxConcurrentJobs`, unless
`admission.runtimeCapacity.maxConcurrentJobs` overrides it. An administrator can
edit this ConfigMap directly while the server is running; allow for Kubernetes'
projection delay plus the five-second poll. A later Helm reconciliation restores
the value rendered from chart values. Set `enabled: false` to use the boot flag
alone on the next rollout. No ConfigMap API permission is added to the server.

Every accepted value or source change is audited before application as
`admission.capacity.change`, including the previous/effective job values and source. A malformed, missing,
unreadable or oversized key, or an unavailable audit writer leaves the last good value
in force. Before the first accepted override, that value is the boot default.
Errors warn in logs and appear in `system_status.admission.configuration` and
the dashboard's admission-capacity card; they never pause admission. Audit IO
runs outside the admission mutex with a five-second deadline. The queue keeps
using its current budget while an update is awaiting audit.

Lowering capacity never cancels a granted/retained run. Existing workflow usage
may exceed the new ceiling until grants are released; new work waits for room.
Raising capacity wakes the queue. The disk floor, hard IO-pressure valve,
priority, concurrency groups and repository VM limits still apply. Oberth does
not drain workflows to satisfy old size weights or numeric resource arithmetic.

The effective value, boot default, source, file, last accepted change time and
last reload error are exposed with the aggregate admission status. Scoped
status continues to omit host-wide capacity and configuration. The file key is
limited to 64 bytes and its invalid contents are not reflected in diagnostics.

For release qualification, use an [administrative admission hold](admission-hold.md)
to keep ordinary work queued while selected classes proceed. The hold selects allowed classes independently of the workflow count ceiling.
