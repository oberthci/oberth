# Unreleased: v0.17.96

## Branch deletion never blocks; batch prune; outbox repair (#1490)

**P1 fix.** Since v0.17.84 a `git push --delete <branch>` could hang the
receive indefinitely, and replaying such a deletion at startup could exhaust
the scheduler's two-minute recovery budget and stop the server (a crash loop
under the Recreate strategy). Receive callbacks run while the repository's
receive lock is held, and the deletion handler (`DeleteBranch`) took the same,
non-reentrant lock again before pushing the deletion upstream. Branch deletion
is now asynchronous:

- The receive deletes the mirror ref, records a pending upstream deletion in
  the same transaction as the receive event, and returns. The callback never
  takes the receive lock and never touches the network (a deletion of a
  registered repository no longer refreshes from the forge), so startup replay
  of a deletion completes within its budget.
- A background propagator deletes upstream refs with one push per repository
  batch and no repository lock held across network I/O. The upstream ref is
  deleted only while it still names the deleted head or an ancestor
  (force-with-lease). Failures retry with backoff (1, 2, 4, 8, 16 minutes); after
  six attempts, or when the upstream ref moved, the record is `failed` and the
  mirror branch is restored from its upstream tracking ref.
- Per-branch state (`pending`, `propagated`, `failed`, `skipped`) is in the new
  admin MCP tool `branch_deletion_list` and `oberth branch deletions`. The push
  notice now reads "deleted on the mirror · upstream deletion queued". Each
  propagation pass is audited as `branch.delete.upstream`.

**New MCP tools (75 → 78), unscoped admin only:**

- `branch_delete_many` / `oberth branch prune`: dry run by default. The plan
  lists each branch's head SHA, merged/patch-equivalent state, ahead/behind
  counts and last commit date, plus a `plan_id`. Select an explicit
  `{branch, expected_sha}` list, or `merged_into` (the default branch) and/or
  `older_than` with `exclude_prefixes`. `confirm: true` with the same arguments
  and `plan_id` executes; any moved head makes the plan stale. Each ref is
  compared and deleted in one local transaction, and unmerged branches need
  `allow_unmerged`. Each batch writes one `branch.prune` audit entry, and
  upstream propagation is batched and asynchronous. A plan holds at most 1000
  branches; patch equivalence is checked for at most 500 unmerged heads.
- `branch_deletion_list`: per-branch receipts by repository, status, prune batch
  or receive event.
- `receive_outbox` / `oberth outbox list|show|retry|abandon`: repair a receive
  record that blocks its repository. `retry` delivers it (an operator-set
  `quarantined` state is restored first when the rest of the record validates);
  `abandon` needs the record id and a reason, moves the file to
  `.receive-outbox/abandoned/` and is audited.

**Protected branches.** Each repository's default branch, plus the globs of the
new `--protected-branches` server flag (chart `branches.protected`, default
`main,master`), cannot be deleted by `git push --delete` or by prune selectors.
An admin can delete exactly one such branch with `branch_delete_many` and a
typed `confirm_protected: "<upstream>/<org>/<repo>:<branch>"`. **Behaviour
change:** a push deleting a non-default `main` or `master` branch is now
refused; set `branches.protected` to override the default list.

**Upgrade and compatibility.**

- New forward migration `20261107120000-branch-deletions` (table
  `branch_deletions`). Existing migrations are unchanged.
- Outbox records keep their format. Records an operator marked `quarantined` are
  still skipped at startup until repaired with `oberth outbox`.
- Old clients pushing `--delete` keep working; only the upstream push moves out
  of the receive.
- To finish the deletions stranded by the v0.17.95 upgrade (upstream still has
  the branches), copy each backed-up record back into `.receive-outbox/` with its
  `quarantined` state, then run `oberth outbox retry <upstream/org/repo>`.
  Retrying re-delivers the deletion, and the propagator removes the upstream
  branch.

**Follow-up (not in this release):** the per-repository `branch_retention` option
(scheduled deletion of merged branches) and keeping reads served while startup
replay retries.
