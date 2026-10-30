# Host pressure admission

Oberth can defer **new resource grants** while the host is under sustained IO
pressure. This helps avoid starting more work during an overloaded disk wave.
It does not pause, kill, resize or revoke a running workflow, retained grant, or
adopted workflow. It is a scheduling signal, never a run verdict or a readiness
or liveness failure.

The server reads the Linux kernel's host-wide `/proc/pressure/io`, `cpu`, and
`memory` files in the admission path. PSI percentages describe time tasks spent
stalled; `io full avg60` is the sixty-second average for all non-idle tasks
being stalled on IO, rather than device utilization or free space. CPU gating
uses `some avg60`; host CPU `full` is undefined and is exposed as zero. Memory
PSI is diagnostic only. See the [kernel PSI interface documentation](https://docs.kernel.org/accounting/psi.html).

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
must be positive when enabled. For fractional Helm CLI values, use a YAML values
file or e.g. `--set-json admission.pressure.ioFullAvg60Max=35.5`; Helm's plain
`--set` treats decimals as strings, which the schema rejects. Explicit zeros are
preserved during chart rendering, including reused values.

IO holds when `full avg60 >= 30` and resumes only below `20`. CPU, when enabled,
has an independent latch using its own threshold minus the same hysteresis.
Either resource can hold grants. One recovering resource cannot clear the
other's hold. A valid low reading clears the pressure-wait episode; the next
high episode gets a fresh timer.

A run's timer starts the first time it would otherwise receive a new grant but
pressure holds it. At 20 minutes it may pass the pressure check despite ongoing
high readings. This bypass does **not** bypass priority, group ownership, VM
capacity, weighted capacity, run ceilings, disk-space floors, XL isolation, or
backfill draining. The grant pass stops at the pressure-held candidate; lower
priority work cannot use an older pressure timer to jump it. Release/tag
priority and group ordering stay as before. Continuous higher priority traffic
still follows the existing priority aging policy. The timer is in memory and
restarts with the process; this adds no new persistent run state.

The check bounds workflow admission, after immutable checkout and static parsing
have determined the exact resource contract. Those preparatory reads and
existing Git refreshes are not themselves throttled by this gate. The maximum
pressure wait is additional to other capacity waits; it is not a start-time SLA.

Readings are cached for at least one second and refreshed by admission passes,
including the existing 15-second dispatch poll when idle. Source reads are three
bounded procfs reads (at most 16 KiB each); status and metrics do no procfs IO.
Sample time is exposed so consumers can detect staleness. Unsupported platforms,
unavailable files, malformed/non-finite/out-of-range samples, missing mandatory
rows, or oversized files fail open to the previous admission behavior. The
process logs one source warning, clears stale latches/timers, and keeps trying
on later admission passes. A later valid sample restores protection. No host
mount, elevated privilege or Kubernetes API query is required.

Unscoped `/api/status` exposes `admission.pressure`: thresholds, hysteresis,
max defer, availability, observation time, the three PSI samples, holding
resource, and the count of directly pressure-held eligible reservations. The
head reservation and its per-run scheduling observation expose the deferral
reason and origin, e.g. `deferred: io pressure 42.5%`. Other queued reservations
can still wait behind that head. The dashboard shows the reason and a host
pressure status card. Upstream-scoped views retain the existing host-load
privacy rule and omit PSI percentages, aggregate state and pressure reasons.

The existing authenticated admin metrics endpoint adds:

- `oberth_admission_pressure_avg60{resource="io|cpu|memory",kind="some|full"}`
- `oberth_admission_pressure_enabled`, `_available`, `_holding`, `_deferred`
- `oberth_admission_pressure_deferrals_total`, `_bypasses_total`

The gauges reflect the last admission sample; averages are absent without a
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
