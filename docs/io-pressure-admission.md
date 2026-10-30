# Host pressure admission

Oberth can defer **new resource grants** while the host is under sustained IO
pressure. This helps avoid starting more work during an overloaded disk wave.
It does not pause, kill, resize or revoke a running workflow, retained grant, or
adopted workflow. It is a scheduling signal, never a run verdict or a readiness
or liveness failure.

The server reads its own node's Linux host-wide `/proc/pressure/io`, `cpu`, and
`memory` files in the admission path. PSI percentages describe time tasks spent
stalled; `io full avg60` is the sixty-second average for all non-idle tasks
being stalled on IO, rather than device utilization or free space. CPU gating
uses `some avg60`; host CPU `full` is undefined and is exposed as zero. Memory
PSI is diagnostic only. On a multi-node cluster this is the server node, which
may differ from the workflow node; this gate is intended for a shared single
host. Sandboxed runtimes that hide procfs fail open. See the [kernel PSI interface documentation](https://docs.kernel.org/accounting/psi.html).

Default Helm values (also the `serve` defaults):

```yaml
admission:
  pressure:
    ioFullAvg60Max: 30
    cpuSomeAvg60Max: 0
    hysteresis: 10
    maxDefer: "20m"
```

The equivalent flags are:

- `--admission-pressure-io-full-avg60-max=30`
- `--admission-pressure-cpu-some-avg60-max=0`
- `--admission-pressure-hysteresis=10`
- `--admission-pressure-max-defer=20m`

Each zero threshold disables that resource. Set both to zero to disable the
check entirely. Percentages must be finite and between 0 and 100. Hysteresis is
in percentage points and must be less than every enabled threshold. `maxDefer`
must be positive when enabled. Helm also rejects incompatible threshold/hysteresis
pairs and zero-duration maximum deferrals at render time. For fractional Helm CLI values, use a YAML values
file or e.g. `--set-json admission.pressure.ioFullAvg60Max=35.5`; Helm's plain
`--set` treats decimals as strings, which the schema rejects. Explicit zeros are
preserved during chart rendering, including reused values.

IO holds when `full avg60 >= 30` and resumes only below `20`. CPU, when enabled,
has an independent latch using its own threshold minus the same hysteresis.
Either resource can hold grants. One recovering resource cannot clear the
other's hold. A valid low reading clears the pressure-wait episode; the next
high episode gets a fresh timer.

Every prepared waiter gets its own pressure timer while a hold is active,
except waiters parked on a concurrency group, VM or KubeVirt capacity. Weight
or other resource waits do not serialize these clocks. With four unparked jobs
queued together and room for all four, all four may pass pressure at about 20
minutes (within the next dispatch poll), in existing priority/FIFO order. This
is a bound on each run's pressure wait, not a rate limit of one job per 20m.

The grant pass still stops at its first pressure-held candidate. Expiry bypasses
**only pressure**, never priority, group ownership, VM capacity, weighted
capacity, run ceilings, disk floors, XL isolation or backfill draining. A newer
high-priority head with a fresh timer can still delay an expired lower class;
existing priority aging remains in force. A recovery or invalid sample resets
queued pressure episodes. A VM-parked run that previously held a grant retains
its clock, so re-acquisition cannot restart an already exhausted pressure wait.
Timers are in memory and restart with the process; no persistent run state is
added. Capacity/priority waits can exceed maxDefer.

The check bounds workflow admission, after immutable checkout and static parsing
have determined the exact resource contract. Those preparatory reads and
existing Git refreshes are not themselves throttled by this gate. Pressure
clocks can overlap resource waits; expiry only removes the pressure hold. Other
gates can still delay a run, so maxDefer is not a start-time SLA.

Readings are cached for at least one second and refreshed by admission passes,
including the existing 15-second dispatch poll when idle. Source reads are three
bounded procfs reads (at most 16 KiB each); status and metrics do no procfs IO.
Sample time is exposed so consumers can detect staleness. Unsupported platforms,
unavailable files, malformed/non-finite/out-of-range samples, missing mandatory
rows, or oversized files fail open to the previous admission behavior. The
process logs one source warning per outage (re-armed on recovery), clears stale latches/timers, and keeps trying
on later admission passes. A later valid sample restores protection. No host
mount, elevated privilege or Kubernetes API query is required.

Unscoped `/api/status` exposes `admission.pressure`: thresholds, hysteresis,
max defer, availability, observation time, the three PSI samples, holding
resource, and the count of all prepared, unparked waiters whose own pressure
clocks have not expired. The
head reservation and its per-run scheduling observation expose the deferral
reason and origin, e.g. `deferred: io pressure 42.5%` for an admin HTTP view.
Non-admin and MCP run views retain the actionable non-numeric reason
`deferred: host io pressure`; percentages, pressure timestamps and pressure
max-wait numbers are omitted there. Scoped status also retains this cause on
visible reservations, while omitting the global PSI sample and aggregate state.
The dashboard handles both forms and shows the full host status card where
aggregate status is available.

The existing authenticated admin metrics endpoint adds:

- `oberth_admission_pressure_avg60{resource="io|cpu|memory",kind="some|full"}`
- `oberth_admission_pressure_enabled`, `_available`, `_holding`, `_deferred`
- `oberth_admission_pressure_deferrals_total`, `_bypasses_total`

Metrics declare Prometheus gauge/counter types. The gauges reflect the last admission sample; averages are absent without a
valid sample. Counters reset with the process, count once per pressure episode
or granted bypass, and have no run/repository labels. Transition logs report
source recovery and hold changes; each directly deferred run and bounded bypass emits
one line. A slow metrics consumer does not hold the admission mutex while the
response is written.

This default introduces admission delay under high pressure. Operators can
change the thresholds or disable both gates while inspecting the observed
values. IO pressure does not identify its source, reserve disk bandwidth, or
guarantee tests will meet their deadlines. The independent free-space floor
still protects disk capacity and is never waived by `maxDefer`.

During a long IO hold, lower admission capacity; do not raise it to clear the
hold. More concurrent work can worsen disk contention when the hold clears or
individual pressure clocks expire. Lowering does not cancel running grants;
existing work drains before the smaller capacity admits more. Follow the
[runtime capacity procedure](runtime-admission-capacity.md) and check the
effective configuration in Status.
