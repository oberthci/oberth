# Declared resources — Stage B

Git owns resource values. Oberth validates explicit declarations and submits
them unchanged. It adds no CPU limit, does not fill missing requests, and never
raises resources after an OOM or deadline failure. Existing defaults and runtime
behavior for missing declarations stay unchanged.

## Administrator ceilings

The existing durable `size_grants` record selects numeric **per-container**
ceilings. This is independent of the size annotation in the pipeline and applies
to every fresh submission, including releases and protected preparation.

| Administrator maximum | CPU | Memory | Ephemeral storage |
| --- | ---: | ---: | ---: |
| L, including no active grant | 4 | 8Gi | 16Gi |
| XL | 8 | 16Gi | 32Gi |

Both requests and limits must fit. L uses half of the existing global maxima;
XL reaches those global maxima. These are declaration ceilings, not a claim
about host capacity or measured usage. They do not add limits to a container
that omitted them. VM allocations retain their separate grants and bounds.
A step with no limit remains unbounded, as it is today. The ceiling validates
explicit declarations per container; it does not cap a whole workflow's resource
use or account for simultaneous containers. Enforcement through aggregate
CPU/memory admission comes with Stage C.
The existing server workflow timeout also bounds explicit step deadlines;
positive integer step deadlines are required when declared.

An administrator can select XL with `oberth size grant repo-01 --max XL`.
`size_grant_get` returns the numeric `resource_ceiling` alongside the existing
grant record. Revocation returns to L for new submissions. Each fresh submission
reads its own repository ID's grant; a read error refuses submission. The
decision is snapshotted for validation, audit and submission. Already running
workflows retain their resources; restart reconstruction does not resize them.

Ceilings cover requests and limits on containers, scripts, container sets,
repository-authored init/sidecar containers, template defaults and nested inline
templates, including resolved fragments. Unused definitions cannot hide values
above the ceiling. Negative declarations and a request above its own explicit
limit are refused. The rejection identifies the repository, template/container,
resource, declared value and administrator ceiling, for example:

```
repo repo-01 template step-abc12345: resources.requests.cpu 5 exceeds administrator L ceiling 4; an administrator must change the size grant, or reduce the declaration in git
```

The check runs in the trusted submission build before server helper injection
and external creation. The same resource policy participates in release
preflight. A pipeline cannot raise it by declaring XL. Existing admission still
refuses workflow/template priority overrides and every repository-authored pod
patch, including defaults and inline templates. Those protections cover both
priority and resource-patch bypasses.

## Messages and history

A step declaration is complete for this message when every main/container-set
container explicitly requests positive CPU and memory, including template
defaults. Other dimensions remain optional and are honored when present.
Coverage counts reachable pod-producing call sites, including root pods and
nested templates, once per fan-out site rather than per runtime item or retry.
Unused templates do not inflate the count. Unsupported or excessive graphs
remain unknown rather than reporting zero missing declarations.

The existing run message reports the number of incomplete declarations and
`oberth run resources --run <id> --include-failed`. This notice is returned in
`resource_message`, separately from the exact status vocabulary in
`display_reason`. The dashboard appends it to the message cell after its usual
status, schedule or release message; restart-warning styling remains intact.
No new run-row labels are added. Counts are
stored as optional numeric metadata in existing workflow timing JSON and read
in indexed batches, through the read pool. Old immutable timing records stay
unknown. Missing telemetry does not prevent execution or fabricate a count.

For a failed final step with a bound OOM diagnostic, the durable run message
names the step, memory limit, external sampled memory peak and
`oberth run resources --run <id> --include-failed`. A deadline message gives the
step deadline, observed container runtime and sampled memory peak. Missing
values say `unknown`. Peaks remain lower bounds; self-reported termination
measurements are not used. A successful retry does not inherit a previous
attempt's failure message. An ambiguous workflow-wide deadline reports the step
as unknown rather than blaming an arbitrary failed step. Normal and restart
finalization use the same code.

After deployment, run branch CI, inspect its resource history, then commit any
resource/deadline changes in the owning workflow alongside the code change.
For example, `oberth run resources --repo repo-01 --step step-abc12345 --yaml`.
The anonymised laptop table remains unchanged and cannot supply suggestions
where attempt identity is ambiguous.

Stage B changes no queue units, admission weights, run ceilings or scheduling
clocks. Stage C and later work remain separate. No schema migration is required.
