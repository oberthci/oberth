# Workflow deadlines

Repository authors control native Argo Workflow `activeDeadlineSeconds`,
template `activeDeadlineSeconds` / `timeout`, retry policies and synchronization.
Oberth does not inject a default, cap an authored value, add startup allowance,
or enforce a workload watchdog or no-progress deadline. A deadline is optional.
Argo anchors Workflow execution to its own `status.startedAt`; controller
parallelism postponement and Kubernetes Pending are native waiting states.

Observations report authored native deadlines and existing queued/running
reasons. Oberth does not terminalize a run for waiting. Cancellation, supersede,
branch deletion and terminal publication/recovery remain lifecycle operations.

Opted-in [assisted continuation](assisted-continuation.md) retains its bounded
hold TTL, retry expression, private scratch and held-attempt deadlines. These
are part of the requested hold lifecycle. Executor credential delivery also
retains identity issuance/observation failure handling; it grants no credentials
to an unverified Pod and does not expire an ordinary unscheduled workload.

The source-seed Pod still has its existing three-minute preparation wait before
the Workflow exists; see [source delivery](native-scheduling.md#source-delivery-and-load).
A seed failure is preparation failure and can qualify for infrastructure retry.
