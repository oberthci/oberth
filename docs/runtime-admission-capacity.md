# Change admission capacity without restarting

`--max-concurrent-jobs` remains the boot default (0–64; 0 is no ceiling).
Optional `--max-concurrent-jobs-file` names a mounted text key with one integer
in that range. The server opens it again every five seconds, following
Kubernetes' ConfigMap symlink updates. The effective value caps granted runs.
Kubernetes decides pod placement from declared requests; setting the ceiling
to one is valid, and zero removes it.

The chart enables `admission.runtimeCapacity.enabled` by default. It creates
`<release-fullname>-admission` with key `max-concurrent-jobs` and mounts its
whole directory read-only and optional, without `subPath` or a rollout checksum.
A missing ConfigMap cannot prevent the server Pod from starting; on that restart
the boot flag is the initial last-good value. Initial data
comes from `maxConcurrentJobs`, unless
`admission.runtimeCapacity.maxConcurrentJobs` overrides it. The ConfigMap is
Helm-owned: editing it with `kubectl` makes the next `helm upgrade` fail with a
server-side-apply field conflict. Set `enabled: false` to use the boot flag
alone on the next rollout. No ConfigMap API permission is added to the server.

## Runtime override without Helm (#1497)

To change capacity while the server runs, use the unscoped-admin MCP tool
`admission_capacity_config` (CLI `oberth admission capacity <1..64>|clear
--reason <reason>`). It stores a server-owned override in the database, which
Helm never renders or applies, so upgrades and runtime changes cannot conflict.
Precedence is: runtime override, then the mounted file, then the boot flag.
The override applies immediately, survives restarts and upgrades, and is kept
by every five-second reload; `clear` hands authority back to the file (or the
boot flag when no file is configured). The value must pass the same 1–64 and
weight-budget validation as the file. Each set or clear requires a reason and is
audited as `admission.capacity.override` / `admission.capacity.override.clear`
with the previous override, followed by the usual `admission.capacity.change`
when it is applied. `system_status.admission.configuration.source` reports
`runtime`, `file` or `boot`. A failed override read keeps the last good value
and reports a reload error, like a malformed file.

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
priority and concurrency groups still apply.

The effective value, boot default, source, file, last accepted change time and
last reload error are exposed with the aggregate admission status. Scoped
status continues to omit host-wide capacity and configuration. The file key is
limited to 64 bytes and its invalid contents are not reflected in diagnostics.

For release qualification, use an [administrative admission hold](admission-hold.md)
to keep ordinary work queued while selected classes proceed. The hold selects allowed classes independently of the workflow count ceiling.
