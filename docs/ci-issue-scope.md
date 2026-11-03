# CI and manual issue scope

Issues stay in Oberth under the owner decision of 2026-11-03. The external
tracker migration is dropped. The migration-only export tool is removed;
ordinary verified database/audit backups remain the history preservation path.

| Surface | Current behavior and direction |
| --- | --- |
| CI projection | Keep repository failure creation, updates and eligible green-result closure. |
| Manual issues | Keep global and upstream-associated manual issues and all existing writes. |
| Reads | Keep scoped list/get/bulk-get and bounded pagination; fetch full bodies through detail reads. |
| Mutation coordination | Keep five-minute locks, lock-aware writes and metadata revisions. |
| Body updates | Keep transactional `expected_body_sha256` conflict checks and bounded body append. |
| History | Keep closed records; close/reopen do not delete issue history. |
| Work statuses and stale | Not implemented. |

Current `issue_append` appends to the
body; it is not a discussion thread. Pipeline filesystem workspace isolation
is a separate subsystem from issue/work tracking.
