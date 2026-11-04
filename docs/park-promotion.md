# Park refs and promotion

Push a reviewed candidate to a park branch when its integration gate should be
one promotion run against the current target. By default, park refs live under
`refs/heads/park/`:

```sh
git push oberth HEAD:refs/heads/park/review
oberth mcp call promote '{"repo":"org/repo","sha":"<full-commit-sha>","branch":"main"}'
oberth mcp call promote_status '{"id":"<returned-promotion-id>"}'
```

Oberth accepts and mirrors a park push upstream, retains an audited receive
receipt, and creates no branch run. Deletion follows the ordinary branch deletion
path. Default branches always retain ordinary branch CI.

A parked promotion requires a full commit SHA reachable from a current park
branch in the same repository. An ancestor of the park tip is accepted; an
unreferenced object, similarly named branch, tag, or private promotion pin is not
parked provenance. Parked sources require `repo`; SHA-only lookup of ordinary
green branch evidence remains available. Normal uplink
repository scopes still apply.

An admin may promote a parked commit directly. A non-admin needs either the
repository opt-in below or an existing admin-issued `promote_grant` for the exact
repository, SHA and target tuple. Grant expiry, revocation, pending reservation
and single delivered use retain their normal checks. Opt-in does not authorize
promotion of an ordinary untested branch.

## Repository policy

Commit `.oberth/promotion.yaml` on the **upstream default branch**:

```yaml
park_ref_prefix: refs/heads/park/
allow_without_branch_ci: true
require_branch_ci:
  - release/*
```

The file is optional. Without it the park prefix is `refs/heads/park/`, repository
opt-in is false, and admins or exact promotion grants can authorize parked
promotions. `park_ref_prefix` must name a namespace under `refs/heads/` and end
in `/`. For example, `refs/heads/review/` replaces the default namespace.

`require_branch_ci` contains target branch names or Go `path.Match` patterns;
`*` matches within one path segment. `main` requires a green branch commit for
main; `release/*` does so for branches such as `release/stable`. This requirement
also retains branch CI on matching pushes, including refs within the park
namespace. An admin or grant cannot bypass it. Normal release tag admission,
annotated-tag provenance, secret isolation, release capability preflight and
trusted test requirements remain in force.

Promotion authorization requires freshly fetched private upstream tracking state.
Changing the file on a candidate or pushing an untested local default branch
cannot enable the opt-in or remove a requirement. Unreadable, malformed, unknown
fields and multiple YAML documents refuse promotion. The file is limited to 16 KiB.
Push admission reuses successful upstream snapshots for up to two seconds and
bounds its policy read to two seconds. On any policy read or validation error,
the push follows ordinary branch CI instead of parking; upstream availability
cannot prevent CI enqueue. Candidate content never supplies the fallback policy.

## Testing, publication and audit

A parked promotion merges onto the current target and runs the full existing
promotion pipeline on that exact tree. A fast-forward, an already-contained
source and a new target branch still receive a promotion run. Red CI never
publishes the target. The final push retains the ordinary non-forced target
movement check.

The run uses the existing status vocabulary and reason line:
`promoted without branch CI`. Promotion records expose `BranchCISkipped`;
`promotion_list` exposes `branch_ci_skipped`. The admission audit action records
`branch_ci_skipped`, and park receive receipts record `push.branch.parked`.
Schema migration 54 adds an immutable boolean, defaulting to false for existing
promotions; historical audit records are unchanged.

`promote_retry` rechecks current parked provenance, repository policy and caller
or grant authority. It tests the merged tree again for a skipped-branch source.
Queued planning and publication recheck target branch-CI requirements. Ordinary
green branch promotions retain their existing fast-forward reuse behavior.
Trusted-preflight recovery retains skipped-branch evidence, its audit marker and
the existing run reason on the replacement. Fresh positive suite evidence cannot
bypass a target policy that now requires branch CI.

Single-source and train admission use the same eligibility check. A terminal
failed or interrupted branch run on the SHA refuses promotion, even when the SHA
is also parked; rerun it or fix it first. Trains can combine green and parked
sources. Each green source still needs ordinary caller/grant authority; parked
sources can also use repository opt-in. Any parked member marks the promotion
and audit record as having skipped branch CI. Target branch-CI requirements apply
to the entire train, and every train runs the combined CI gate before publication.
