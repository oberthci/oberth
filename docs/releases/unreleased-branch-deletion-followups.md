# Unreleased: branch deletion follow-ups (#1500)

From the first production bulk prunes.

- An archived upstream repository no longer undoes a prune. A push the forge
  refuses as archived ("Repo ... is archived", or GitHub's "repository was
  archived so it is read-only") is final after one attempt: the record is
  `skipped` with an "upstream archived" reason line, and the mirror deletion
  and its marker are kept instead of being restored after six retries. Records
  that already failed this way keep their restored mirror branches; prune them
  again to delete them on the mirror only.
- `branch_deletion_list` (and `oberth branch deletions`) `counts` now covers
  every record of the filtered repository, or of all repositories, regardless
  of `--limit`; with `batch` it covers the batch. The new `page_counts` holds
  the counts of the returned page only.
- Failed records can end. They become `skipped` with a reason line, with no
  ref change, when a later deletion of the same branch propagates, when an
  hourly propagator sweep finds the branch gone from the mirror, or through the
  new unscoped-admin, audited MCP tool `branch_deletion_resolve` /
  `oberth branch resolve <repo> --reason <text> [--id <record-id>]...`.
  Without ids it resolves only records whose branch is gone from the mirror
  and lists the rest as `kept`. MCP tools: 78 → 79.
- Every `branch_delete_many` plan entry has a `class`: `merged`,
  `patch-equivalent`, `unique` or `unchecked` (beyond the git cherry budget of
  one plan), and `classes` counts every candidate. `report: true`
  (`oberth branch prune --report`, dry run only) also lists the kept candidates
  under `kept`, so one dry run classifies a whole repository. The plan ID does
  not change with `report`.
- A feature-branch run whose branch was deleted on the mirror while it was
  publishing finishes `passed` with the reason line "branch ... was deleted on
  the mirror; result not published, upstream left unchanged". Only the
  publication is abandoned; previously the green run was marked failed and
  abandoned.
