# CI issue queue and compatibility

Oberth's issue queue records CI failures and their investigation. It is not a
general work tracker. The workspace design, work-status enum, computed stale
field and discussion-thread model are absent from this version. Pipeline
filesystem workspace isolation is unrelated and remains part of execution.

| Surface | Current behavior |
| --- | --- |
| CI projection | Repository failure creation, updates and eligible green-result closure. |
| Manual issues | Existing installation-wide and upstream-associated issues and writes remain available for CI investigation and compatibility. |
| Reads | Scoped list/get/bulk-get and bounded pagination; full bodies through detail reads. |
| Mutation coordination | Five-minute locks, lock-aware writes and metadata revisions. |
| Body updates | Transactional body-hash conflict checks and append into the existing body, not a discussion thread. |
| History | Close/reopen preserve records; verified database/audit backups preserve full history. |

The #1318 audit found two retained general surfaces: an unscoped operator can
create an issue without an upstream, and successive appends can grow its body.
The one-MiB tool-request ceiling limits each incoming mutation, not the total
stored body. A bulk read is bounded to 256 KiB and individual detail reads can
return the complete record. Do not describe these existing semantics as a new
workspace feature or claim an aggregate body limit that is not implemented.

Existing installation-wide records are not automatically reassigned, truncated,
made inaccessible or deleted during this upgrade. Move any organization-specific
planning records to their owner's system before a separately reviewed removal
of those compatibility writes; this release performs no external transfer and
no live issue migration. The removed migration-only exporter and external-tracker
policy are not reintroduced. No schema change is needed for this scope cleanup.
