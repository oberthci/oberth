# Unreleased: v0.17.97

## Helper storage limits follow the Pod Argo runs (#1491 follow-ups)

New Workflows use helper-storage policy `argo-256Mi-512Mi-v4`. Like v3, a
helper keeps its 256Mi ephemeral-storage request and adds its 512Mi limit only
when the step itself declares an ephemeral-storage limit. v4 decides that from
the containers Argo v4.0.8 actually runs after merging `spec.templateDefaults`:

- A default `containerSet` limit no longer counts. Argo replaces container-set
  lists rather than merging them, so that limit never ran, and under v3 the
  step's Pod was capped at the helper limits alone.
- A default sidecar now counts even when the template does not list it, since
  Argo adds default sidecars by name.
- Native sidecars (init containers with `restartPolicy: Always`) now count;
  the kubelet adds their limits to the Pod total like regular containers.
- An explicit `0` limit on the template overrides a default limit.

Running and adopted Workflows keep the policy they were submitted with (v1, v2
or v3) and reconstruct with the same spec identity.

**Rollback:** a v0.17.96 server refuses to re-create Workflows with policy
`argo-256Mi-512Mi-v4`, and v0.17.95 also refuses v3, so drain v4 runs before
rolling back to v0.17.96, and v3 and v4 runs before rolling back to v0.17.95.

## Storage eviction classification

- "Container X exceeded its local ephemeral storage limit" is now a failure of
  the step, not infrastructure, when X is one of the step's own containers with
  a declared limit (the main container, a container-set member, a sidecar or a
  native sidecar). The run is not auto-retried, and the reason names the
  container and its limit, for example `Step build/test exceeded its declared
  ephemeral-storage budget: the kubelet evicted its Pod at container main's own
  limit 640Mi. Raise the step's ephemeral-storage limit in git.` A limit on an
  Argo or Oberth helper (`wait`, `init`) stays infrastructure, as do
  node-pressure evictions.
- The Pod-total message is now recognised with every quantity form the kubelet
  prints, including decimal exponents such as `1e9`.
- For a v3 Workflow whose only limit was a default `containerSet` member, a
  Pod-total eviction is infrastructure again: that total was helper limits
  alone. It gets the single auto-retry, which runs under v4.

## Release signing

The "already signed?" check in `.oberth/release.sh` retries once after 5
seconds before re-signing a registry object, so a transient TUF or Rekor
failure no longer adds a duplicate signature. The final verification is
unchanged.

## Branch deletion never blocks; batch prune; outbox repair (#1490)

**P1 fix.** Since v0.17.84 a `git push --delete <branch>` could hang the
receive indefinitely, and replaying such a deletion at startup could exhaust
the scheduler's two-minute recovery budget and stop the server (a crash loop
under the Recreate strategy). Receive callbacks run while the repository's
receive lock is held, and the deletion handler (`DeleteBranch`) took the same,
non-reentrant lock again before pushing the deletion upstream. Branch deletion
is now asynchronous:

- The receive deletes the mirror ref, writes a deletion marker during receive
  finalization (under the repository lock, so no refresh can re-create the
  branch), records a pending upstream deletion in the same transaction as the
  receive event, and returns. The callback makes no Git call and never touches
  the network (a deletion of a registered repository no longer refreshes from
  the forge), so startup replay of a deletion completes within its budget.
- A background propagator deletes upstream refs with one push per repository
  batch and no repository lock held across network I/O. The upstream ref is
  deleted only while it still names the deleted head or an ancestor
  (force-with-lease). Failures retry with backoff (1, 2, 4, 8, 16 minutes); after
  six attempts, or when the upstream ref moved, the record is `failed` and the
  mirror branch is restored from its upstream tracking ref (as last fetched,
  which may lag the forge until the next refresh). Several pending records for
  one branch share one delete refspec.
- A feature-branch publication whose branch was deleted on the mirror after
  its run started is abandoned (actor `oberth:branch-deletion`) instead of
  force-pushing the branch back upstream.
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
  `allow_unmerged`. Queued rows become due for upstream propagation only after
  the local deletion applied; refused heads are recorded as `skipped`. The dry
  run reads refs under a short lock and computes patch equivalence without it. Each batch writes one `branch.prune` audit entry, and
  upstream propagation is batched and asynchronous. A plan holds at most 1000
  branches; patch equivalence is checked for at most 500 unmerged heads.
- `branch_deletion_list`: per-branch receipts by repository, status, prune batch
  or receive event.
- `receive_outbox` / `oberth outbox list|show|retry|abandon`: repair a receive
  record that blocks its repository. `retry` delivers it (an operator-set
  `quarantined` state is restored first when the rest of the record validates);
  `abandon` needs the record id and a reason, moves the file to
  `.receive-outbox/abandoned/` and is audited.

**Reserved branches.** Each repository's default branch can never be deleted:
not by `git push --delete`, not by prune selectors, and not by an explicit
`branch_delete_many` request, even with a confirmation. The globs of the new
`--reserved-branches` server flag (chart `branches.reserved`, default
`[main, master]`; `[]` protects only the default branch; `null` keeps the server
default) are refused the same way, except that an admin can delete exactly one
such non-default branch with `branch_delete_many` and a typed
`confirm_reserved: "<upstream>/<org>/<repo>:<branch>"`. **Behaviour change:** a
push deleting a non-default `main` or `master` branch is now refused; set
`branches.reserved` to change the list.

**Upgrade and compatibility.**

- New forward migration `20261107120000-branch-deletions` (table
  `branch_deletions`). Existing migrations are unchanged.
- Outbox records keep their format. Records an operator marked `quarantined` are
  still skipped at startup until repaired with `oberth outbox`.
- Old clients pushing `--delete` keep working; only the upstream push moves out
  of the receive.
- To finish the deletions stranded by the v0.17.95 upgrade (upstream still has
  `chore/917-followups` and probably `auto/cli-v0.19.133`), check the mirror
  first, for example with `git ls-remote <mirror> refs/heads/<branch>`:
  - Branch absent on the mirror: copy the backed-up record back into
    `.receive-outbox/` with its `quarantined` state, then run
    `oberth outbox retry <upstream/org/repo>`. Retrying re-delivers the
    deletion, and the propagator removes the upstream branch.
  - Branch present again (a refresh re-materialized it from upstream): do not
    restore the record. Delete it with `oberth branch prune <repo> --branch
    <name>=<sha>` (dry run, then `--confirm --plan-id`); upstream follows.

**Follow-up (not in this release):** the per-repository `branch_retention` option
(scheduled deletion of merged branches) and keeping reads served while startup
replay retries.
