# Inactive conductor lifecycle

This package implements the operator's conductor Job lifecycle against the
schema-14 pilot journal. It is not connected to scheduler dispatch. It has no
attach, exec, log-reader, guest-launch, fixture-delivery or success-receipt path.

`Create` persists the closed Job intent and wins the durable submit transition
before issuing one Kubernetes create. An ambiguous submission is only observed
on retry. It retains capacity until an exact late UID is bound and cleanup is
observed; NotFound alone cannot discharge an unbound create.

`BindAttempt` verifies the complete actual Job and Pod specs, the Pod's Job owner
UID, the sole container, its exact image digest, container ID and start time.
It retains every observed owned Pod before refusing additional attempts. It
does not trust labels or a claimed-spec annotation as execution authority.
`Termination` re-observes that process and its actual container exit. Job success,
Pod phase and guest output cannot establish a successful exit. A valid exit is
still provisional and cannot create a suite receipt.

`Observe` and `Delete` implement the existing pilot cleanup interface for the
conductor Job and its Pods only. Namespace Pod inventory uses owner UIDs without
a label selector, including after parent disappearance. Unsafe owned Pods are
retained as cleanup obligations without authorizing their execution. Deletion
requires durable cleanup state and an exact UID precondition. The reconciler
must independently observe absence before releasing capacity. Unknown fields,
changed recorded specs, incomplete inventories and API failures hold cleanup.
The API's foreground-deletion finalizer is accepted only on an object marked
for deletion; Pod Job-tracking finalizers are also recognized. No finalizer is
removed by this adapter.
This initial adapter refuses namespace inventories above 32 Pods rather than
claiming absence from a truncated list.

The closed producer contract is one Linux/amd64 image containing
`/usr/local/bin/beacon-conductor`, executable by UID/GID 65534 with a read-only
root, no volumes or environment, no ServiceAccount token, one CPU and 256 MiB.
The image is digest pinned. Runtime ImageID must be the exact admitted digest,
the exact admitted image reference, or that reference with `docker-pullable://`.
Other runtime ImageID encodings fail closed until independently reviewed.

Kubernetes timestamps serialize to whole seconds, whereas journal admission
retains nanoseconds. Lower-bound comparisons against admission use second
precision only for observations with zero nanoseconds. The actual observed
timestamps and precise journal timestamps are retained unchanged. Earlier
seconds, precise observations before admission, future starts, deadlines and
process replacement checks still fail closed.

Only explicit inert API defaults are normalized: the matching ServiceAccount
alias, default preemption/zero priority, scheduled node assignment, the two
standard 300-second unreachable/not-ready tolerations, and equivalent resource
quantities. Producer contents, real Kubernetes admission/defaulting behavior,
runtime image reporting, network isolation, bounded stream attach and the
protected provider remain separate acceptance requirements. Fake-client tests
do not establish live execution or release readiness.
