# Readiness under load

The readiness observer uses a reserved single-connection read-only WAL pool,
separate from the writer, ordinary list/mapping readers, audit readers, SSH
authentication and upgrade-idle sampling. Upstream enumeration uses that reader;
the dedicated-key configuration check reuses those sampled rows. HTTP `/readyz`
reads the cached sample and process-local startup gate without database I/O.

Sampling runs serially on a five-second ticker with a ten-second budget.
The last successful sample remains valid for 90 seconds of elapsed time,
independent of the number of attempts. A ticker drops missed ticks, so ten-second
reads run about every ten seconds (roughly nine attempts in 90 seconds). A
deadline retains that sample without renewing its age. Slow successful samples
renew it. Hard database/configuration errors, no upstream,
explicit cancellation and incomplete startup recovery still refuse readiness;
timeouts cannot make an uninitialized or failed deployment ready. A stopped
observer or persistent deadlines eventually expire the successful sample.
This is bounded time-based hysteresis for CPU/storage scheduling delays. The
readiness reader's SQLite busy timeout is at least the ten-second probe budget;
a longer configured busy timeout is preserved on every replacement connection.

The sampler logs the first retained deadline after each successful sample,
including sample age and database wait, while counting every retained deadline.
`/readyz` logs a stale transition even while the sampler is stuck, once per
transition. These logs contain fixed reason classes, not database/credential
error details. The authenticated administrator-only `/metrics` endpoint exports
`oberth_readiness_sample_present`, `oberth_readiness_sample_age_seconds` and
`oberth_readiness_retained_deadlines_total` without storage I/O.

The chart keeps a five-second HTTPS readiness timeout and five-second period,
and requires six consecutive failures (about 30 seconds) before withdrawing the
endpoint. This also tolerates TLS handshake/scheduling delays independently of
the cached sample. It does not change liveness or startup probes.

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
