# Ordinary tag runs and release tags

Tags carry an object SHA and its peeled tested commit. Tag identity is separate
from release authority:

| Tag name / policy | Workflow | Release authority |
| --- | --- | --- |
| `v<major>.<minor>.<patch>` followed by any suffix | `.oberth/release.yaml`, or an administrator-selected release document | Release admission |
| An explicit administrator release-tag route | The selected release document | Release admission |
| Any other tag | `.oberth/build.yaml` | Ordinary CI |

Every path retains immutable tag names and fresh upstream default-branch
ancestry checks. Annotated tag provenance remains mandatory for release
admission. Ordinary tags may be annotated or lightweight; annotated tags retain
both the tag object and the peeled tested commit. Existing CI secret approvals
apply to ordinary CI; a tag name or workflow parameter grants no release
ServiceAccount, release WIF capability or release workflow access.

## Tags created by promotion

After an admin's `tag_after` promotion passes, Oberth verifies the target still
contains the promoted result and that the result is reachable from the fresh
upstream default branch. It pushes the annotated tag, verifies the exact
upstream object, and passes it through the same tag ingestor and queue admission
as a received tag. The run has `ref_kind=tag`, `trigger=tag`, `release=false`,
the original tag object SHA, the promoted tested commit and the requesting actor.

The deterministic receive ID `promotion-tag:<promotion-id>` makes admission
idempotent. If publication succeeds but admission fails, the
existing publication retry schedule retries admission. If admission commits and
settlement crashes, the stored receive reuses that run without another push,
even if an upstream branch subsequently moves. `tag_after.status=pushed` means
publication and CI admission succeeded; the reason names the queued run. It does
not assert that the tag's CI has passed. Follow `run_wait` / `events_wait` for its
own result. Ordinary tag CI emits normal run events, never `release.published`.

No tag is created after a failed promotion, an invalid request, missing admin
authority, unavailable admission wiring, an ancestry refusal, a conflicting tag
or a refused audit gate. Transient failures keep the durable request pending on
the existing retry schedule. Version names and administrator release routes are
refused by `tag_after` before ref changes. No new run status is introduced.

This change does not add forge polling or webhooks. A tag received through
Oberth's Git transport or published by `tag_after` reaches admission. An arbitrary
ref change made directly on a forge must reach a configured admission source;
merely fetching or discovering that ref does not create a run.

Admin retry/rerun checks still apply to immutable tag runs. A rerun of ordinary
tag CI retains `release=false` and runs `build.yaml`; it cannot become a release
by retrying, changing the workflow's `oberth-trigger` parameter, or using force.
