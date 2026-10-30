# Readiness under load

The readiness observer uses a reserved single-connection read-only WAL pool,
separate from the writer, ordinary list/mapping readers, audit readers, SSH
authentication and upgrade-idle sampling. Both upstream enumeration and the
dedicated-key configuration fallback use that reader. HTTP `/readyz` reads the
cached sample and process-local startup gate without database I/O.

Sampling runs serially on a five-second ticker with a ten-second budget.
The last successful sample remains valid for six budget-plus-cadence windows,
90 seconds. A deadline retains that sample without renewing its age. Slow
successful samples renew it. Hard database/configuration errors, no upstream,
explicit cancellation and incomplete startup recovery still refuse readiness;
timeouts cannot make an uninitialized or failed deployment ready. A stopped
observer or persistent deadlines eventually expire the successful sample.
This is bounded time-based hysteresis for CPU/storage scheduling delays.

The combined API/SSH Service and its readiness gating remain in place. No
NodePort handoff, DNS change or host-key pin migration is required. A genuine
hard failure or an expired sample can still remove the single Pod's endpoint,
including SSH. Authentication continues to use fresh reads from its own pool,
and Git policy and audit/admission checks remain enforced on each operation.

For a single replica, withdrawing the only endpoint cannot route traffic to a
healthier replica. A future availability policy could consider
`publishNotReadyAddresses` to retain routing during dependency-readiness
failures. That would also route requests before readiness and during outages,
so it needs a separate decision about startup, shutdown and operation gates.
This release does not enable it on the combined API/SSH Service.
