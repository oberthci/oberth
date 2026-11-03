# Host pressure admission

Oberth gates new runs by both declared resource budgets and host CPU/IO pressure
by default. Running and adopted workflows keep their grants. This gate is for a
single shared host: on a multi-node installation, the server's node may differ
from the execution node.

```yaml
admission:
  pressure:
    enabled: true
    cpuSomeAvg10: 40
    ioSomeAvg10: 25
    hardCpu: 70
    hardIo: 50
    maxWaitSeconds: 1200
```

The equivalent server flags are `--admission-pressure-enabled`,
`--admission-pressure-cpu-some-avg10`, `--admission-pressure-io-some-avg10`,
`--admission-pressure-hard-cpu`, `--admission-pressure-hard-io`, and
`--admission-pressure-max-wait-seconds`. Set `enabled: false` (or the first flag
to `false`) for budget-only admission. New installations and upgrades default
to enforcement. `enabled: true` selects the new policy even when reused chart
values retain the earlier `mode: observe`; use `enabled: false` to opt out.

Admission reads `/proc/pressure/cpu` and `/proc/pressure/io`, retaining both
`some avg10` and `some avg60`. The gate uses **some avg10**, so it reacts to
shorter contention waves than avg60. CPU must be strictly below 40% and IO
strictly below 25%. Each resource has its own latch: after it blocks, it resumes
only strictly below its threshold minus ten percentage points (CPU 30%, IO 15%
with the defaults). One recovering resource cannot clear the other's latch.

These starting thresholds hold new work when CPU stalls occupy two fifths or IO
stalls one quarter of recent wall time. They leave room for running work to
drain, while ten-point hysteresis avoids repeated starts near the boundary.
PSI measures stall time, not CPU utilization or the load average. The defaults
need qualification against representative workloads; they cannot guarantee a
load-average ceiling, particularly when other processes share the host.

A server-classified release or tag can bypass pressure only when no run holds
an admission grant. Promotions and ordinary branch runs have no such bypass,
regardless of their priority. The first eligible queue head that has waited
**longer than** `maxWaitSeconds` can pass pressure only with CPU below `hardCpu`
and IO below `hardIo` (70% and 50%). Its durable queue timestamp, rather than
the start of a pressure episode, determines the wait. Budget, disk, run-count,
VM, group, priority and backfill rules always apply. A blocked follower cannot
use starvation to jump a fresh head. Other admission gates can exceed maxWait.

The avg10 policy reads bounded procfs files before each grant, including multiple
grants in one scheduling pass. Status and metrics use the last admission sample
and never read procfs. The existing dispatch poll retries holds and source
outages. Missing, malformed or invalid PSI fails open to budget-only admission;
status says `PSI unavailable; budget-only admission`. One warning is logged per
outage, and valid readings restore protection without restarting the server.

The [Linux PSI documentation](https://docs.kernel.org/accounting/psi.html)
distinguishes the system-wide `/proc/pressure` interface from per-cgroup
`cpu.pressure` and `io.pressure`. A standard Linux pod procfs reports the host
values, while sandboxed runtimes can virtualize them. Qualify the actual runtime
by reading the CPU and IO files in the Oberth pod and on its node at nearly the
same time; compare the cumulative `total` values as well as the averages.
Do not use the pod's cgroup directory as a node-pressure source.

If a runtime virtualizes or hides these files, a reviewed chart override can
mount node `/proc/pressure` read-only over `/proc/pressure` in the server container:

```yaml
# Add to the rendered Deployment's pod spec and server container respectively.
volumes:
  - name: node-pressure
    hostPath:
      path: /proc/pressure
      type: Directory
# server container:
volumeMounts:
  - name: node-pressure
    mountPath: /proc/pressure
    readOnly: true
```

This is an opt-in deployment patch, not a default privileged mount. Without a
verified host source the availability fallback keeps the queue moving; it does
not prove node pressure protection. Co-locate execution and server pods when
using this single-host policy.

Admin status exposes the sample, soft/hard thresholds, wait bound, holding
resource (`cpu`, `io`, or `cpu+io`) and fallback reason. `oberth admission` and
admin scheduling messages display CPU and IO numbers and the blocking pressure
gate. Existing scoped and non-admin views retain the actionable resource reason
without host-wide numbers.

Authenticated admin metrics include:

- `oberth_admission_pressure_avg10{resource="cpu|io|memory",kind="some|full"}`
- `oberth_admission_pressure_avg60{resource="cpu|io|memory",kind="some|full"}`
- `oberth_admission_pressure_enabled`, `_enforcing`, `_threshold_exceeded`,
  `_available`, `_holding`, `_deferred`
- `oberth_admission_pressure_deferrals_total`, `_bypasses_total`

CPU full PSI is undefined at host scope and reported as zero. Memory and full
averages are diagnostic; they do not control this policy. No run/repository
metric labels are added. Counters reset on restart.

The earlier avg60 flags and chart fields (`ioFullAvg60Max`, `cpuSomeAvg60Max`,
`hysteresis`, `maxDefer`) remain accepted for compatibility. The public defaults
set both legacy thresholds to zero. Enabled avg10 admission uses fixed ten-point
hysteresis and its own hard-limited queue-head starvation rule; legacy fields
(including `mode: observe`) do not alter that rule. Existing direct Go callers that configure only the
legacy fields retain their previous avg60 behavior. Update callers to
`DefaultAdmissionPressureConfig()` to select the new policy.
