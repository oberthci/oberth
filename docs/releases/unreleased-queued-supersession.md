# Queued branch runs follow the latest pushed head

A new branch-head push now supersedes older queued, not-started runs in the same
repository/ref/schedule lane at every priority, including runs an admin lowered
to low or standard. The replacement keeps the most recent explicit admin
priority, actor and reason, with a fresh queue position. Running runs keep their
existing supersession rules. Tags, promotions and pending publications retain
their existing protection.

The push confirmation counts superseded queued runs even when no Workflow needs
cleanup. Replayed receives retain that count without repeating the enqueue.
