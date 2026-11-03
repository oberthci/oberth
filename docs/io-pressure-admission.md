# Host pressure admission

Oberth gates new runs by both declared resource budgets and host CPU/IO pressure
by default. Running and adopted workflows keep their grants. This gate is for a
single shared host: on a multi-node installation, the server's node may differ
from the execution node.

```yaml
admission:
  pressure:
    enabled: true
    cpuSomeAvg10: 30
    ioSomeAvg10: 20
    hardCpu: 50
    hardIo: 40
    maxWaitSeconds: 1200
```

The equivalent server flags are `--admission-pressure-enabled`,
`--admission-pressure-cpu-some-avg10`, `--admission-pressure-io-some-avg10`,
`--admission-pressure-hard-cpu`, `--admission-pressure-hard-io`, and
`--admission-pressure-max-wait-seconds`. Set `enabled: false` (or the first flag
to `false`) for budget-only admission. New installations and upgrades default
to enforcement. `enabled: true` selects the new policy even when reused chart
values retain the earlier `mode: observe`. The server logs a startup warning
for that explicit legacy setting; use `enabled: false` to opt out.

Admission reads `/proc/pressure/cpu` and `/proc/pressure/io`, retaining both
`some avg10` and `some avg60`. The gate uses **some avg10**, so it reacts to
shorter contention waves than avg60. CPU must be strictly below 30% and IO
strictly below 20%. Each resource has its own latch: after it blocks, it resumes
only strictly below its threshold minus ten percentage points (CPU 20%, IO 10%
with the defaults). One recovering resource cannot clear the other's latch.

These starting thresholds hold new work when CPU stalls occupy three tenths or IO
stalls one fifth of recent wall time. They leave room for running work to
drain, while ten-point hysteresis avoids repeated starts near the boundary.
PSI measures stall time, not CPU utilization or the load average. The defaults
need qualification against representative workloads; they cannot guarantee a
load-average ceiling, particularly when other processes share the host.

A server-classified release or tag can bypass pressure only when no run holds
an admission grant. Promotions and ordinary branch runs have no such bypass,
regardless of their priority. The first eligible queue head deferred by pressure
for **longer than** `maxWaitSeconds` can pass pressure only with CPU below
`hardCpu` and IO below `hardIo` (50% and 40%). The clock starts at its first
eligible pressure deferral, survives brief hysteresis recoveries, and excludes
earlier waits for capacity, a VM, or queue position. At most one starvation
grant is allowed per scheduling pass and per settle window. Budget, disk, run-count,
VM, group, priority and backfill rules always apply. A blocked follower cannot
use starvation to jump a fresh head. Other admission gates can exceed maxWait.

After each grant, the next grant requires a pressure reading taken at least
30 seconds later, allowing pod startup and PSI averages to settle. Consecutive
grants can skip this window only while CPU some avg10 is strictly below 15%
and IO some avg10 is strictly below 10%. Starvation grants always wait for the
full window; an idle release/tag bypass also respects settling.

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
Store comparisons in the installation's operator records. A match establishes
source scope, not a load ceiling. Do not use a Pod cgroup directory as the
node-pressure source.

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
resource (`cpu`, `io`, `cpu+io`, or `settle`) and fallback reason. `oberth admission` and
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

## Declared budgets and cluster allocatable

At startup and on unscoped `system_status` reads, Oberth compares the declared
CPU and memory budgets with the summed allocatable resources of Ready,
schedulable Kubernetes nodes. `admission_capacity` reports both budgets, both
totals, the node count, the overcommit factor, and warnings. The node query has a
three-second deadline. An unavailable query produces an explicit warning;
it never changes admission or readiness. Scoped status omits cluster capacity.
The chart grants the server only `list` on nodes for this check.

The warning threshold is allocatable × `admission.overcommitFactor`, default
`1.0` (server flag `--admission-overcommit-factor`). Set a finite value at least
`1.0` explicitly to opt into overcommit; for example `1.5` permits a 96 GiB
budget on 64 GiB allocatable without warning. A 96 GiB budget on 61.8 GiB still
warns at `1.5`. The factor affects warnings only: it neither raises nor clamps
the configured admission budgets, reservations, or pod requests/limits.

These totals are an upper bound, not a placement guarantee. Taints, affinity,
per-node fragmentation, competing workloads, and platform reserve can reduce
usable capacity. Size budgets for the execution nodes and retain platform
headroom even when cluster totals pass the check.
