# Four execution-priority classes

Next-release change for #1481, based on the reviewed v0.17.85 source. It does
not change that release candidate or restart its CI.

Waiting admission orders `high > promotion > standard > low`, FIFO within each
class. Ordinary builds use standard. Promotions and eligible tag/releases use
promotion, as do configured RC branches. Standard work waiting 60 minutes joins
promotion FIFO; nothing automatically becomes high. High requires an explicit
unscoped-admin or unexpired priority grant. Low supports deferred work.
Existing admin lowering now supports any class and pins that choice. Running
work is not preempted; admission holds, pressure, resources and concurrency
constraints continue to apply.

CLI/MCP/API/dashboard output uses these four names. Legacy normal input maps to
standard; explicit authorized high/highest and their persisted grants map to
high. Automatic historical protection rows never become high authority.
The new timestamp migration extends CHECK vocabularies and retains raw legacy
requests, overrides, grants and audit history. Existing shipped migrations are
unchanged. Back up before upgrade; rollback restores that backup because older
binaries cannot interpret the new class names.

See [execution priority](../release-priority.md) for the exact mapping,
submission authority, tag eligibility, retries and fairness bounds. Publication
and live verification of mixed-class waiting admission remain required for
issue completion; this source change does not close #1481 by itself.
