# Declared resources — Stage B

Git owns resource values. Oberth validates explicit declarations and submits
them unchanged. Missing requests receive the defaults documented in
[workflow admission](declared-admission.md). Oberth adds no CPU limit to
repository containers and never raises resources after an OOM or deadline failure.

## Administrator ceilings

Each repository has an administrator-owned numeric **per-container** ceiling,
expressed as Kubernetes `requests` and `limits`. Workflow size annotations and
usage history cannot select or raise it. It applies to every fresh submission,
including releases and preparation, before server helper injection.

The default is 4 CPU, 8Gi memory and 16Gi ephemeral-storage for each tier. Request
and limit ceilings can differ; each must be positive and requests cannot exceed
limits. Global declaration maxima remain 8 CPU, 16Gi memory and 32Gi storage.
A CPU limit ceiling does not require a workflow to declare a CPU limit.

For example:

```sh
oberth resource grant repo-01 --requests-cpu 1500m --requests-memory 2Gi --limits-cpu 4 --limits-memory 5Gi
oberth resource get repo-01
oberth resource revoke repo-01
```

The corresponding MCP tools are `resource_grant`, `resource_grant_get` and
`resource_revoke`. Grant arguments contain `repo`, `requests` and `limits`; each
tier is an object of Kubernetes quantity strings. CPU and memory are required;
ephemeral-storage defaults to 16Gi when omitted. Mutations require an admin and
are audited atomically. Revocation restores defaults for new submissions.
Lookup failures refuse submission. Validation, audit and submission share the
same snapshot; running workflows retain their declarations.

Migration 49 copies existing size grants exactly: L becomes 4 CPU / 8Gi / 16Gi,
and XL becomes 8 CPU / 16Gi / 32Gi, in both requests and limits. Actor, timestamps
and revocations are preserved. Absent and revoked grants use defaults. After
upgrade, size grants control only deprecated size admission and tag-priority
policy; use resource grants for numeric ceilings. A later size change cannot
overwrite a custom numeric grant. Older binaries refuse the newer schema;
rollback requires restoring the pre-upgrade database backup.

Ceilings validate per-container declarations, independently of the concurrent-workflow ceiling.
They do not insert limits or requests. The server workflow timeout also bounds
explicit positive integer step deadlines.

Ceilings cover requests and limits on containers, scripts, container sets,
repository-authored init/sidecar containers, template defaults and nested inline
templates, including resolved fragments. Unused definitions cannot hide values
above the ceiling. Negative declarations and a request above its own explicit
limit are refused. The rejection identifies the repository, template/container,
resource, declared value and administrator ceiling, for example:

```
repo repo-01 template step-abc12345: resources.requests.cpu 5 exceeds administrator resource ceiling 4; an administrator must change the resource grant, or reduce the declaration in git
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
`display_reason`. It appears in the run detail summary and CLI detail output.
Run lists omit this advice and show only the concise server `display_reason`.
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
The anonymised comparison table remains unchanged and cannot supply suggestions
where attempt identity is ambiguous.

## Workflow admission and placement

Oberth bounds the number of concurrent workflows. Kubernetes places step Pods
using their declared requests and leaves them Pending when capacity is unavailable.
Serving does not reserve a whole-workflow CPU/memory budget, calculate memory
headroom, or fall back to S/M/L/XL weights when declarations are incomplete.

The enabled PSI hard-limit valve and configured disk-space floor can hold new
workflow grants. Priority does not bypass them or the concurrent-workflow ceiling.
Repository concurrency groups, credential checks, numeric per-container ceilings
and ownership/cleanup checks still apply. Existing requests and limits stay in
repository YAML; the read-only resource report never rewrites them.

Missing requests use the documented per-container defaults. Legacy admission
budget, weight and headroom settings remain accepted for upgrade compatibility
and are ignored. See [workflow admission](declared-admission.md) for the exact
defaults, retained controls and Pending/deadline behavior. Numeric resource
ceilings require migration 49; historical demand receipts remain evidence and
do not restore a serving resource-packing budget.
