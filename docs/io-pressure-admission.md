# Host pressure admission

Oberth packs whole Workflow CPU reservations and applies a bounded host CPU/IO
PSI valve. Running and adopted Workflows retain their CPU reservations. The
valve observes the server's host; co-locate server and execution for a
single-host policy. See [CPU admission](cpu-admission.md).

```yaml
admission:
  pressure:
    enabled: true
    hardCpu: 25
    deferSeconds: 300
    hardIo: 40
```

The active flags are `--admission-pressure-enabled`,
`--admission-pressure-hard-cpu`, `--admission-pressure-hard-io` and
`--admission-pressure-defer-seconds`. Disabling PSI leaves CPU request packing,
the disk floor and administrator holds active.

Admission reads bounded `/proc/pressure/cpu` and `/proc/pressure/io` samples.
Hard CPU and IO thresholds use **some avg10**, defaulting to 25% and 40%.
Each resource has an independent latch and recovers strictly below its hard
threshold minus ten percentage points. One recovering resource cannot clear
the other's latch. A fitting run waits at most 300 seconds (configurable), then one run may
start despite PSI. CPU packing, disk floor and administrator holds still
apply. There is no settle window or resource drain forecast.

PSI measures stall time, not CPU utilization or load average. These thresholds
need qualification against representative workloads and cannot guarantee a
load ceiling. Missing, malformed or unavailable PSI retains the existing
fail-open behavior: status reports `PSI unavailable; CPU request admission`.
One warning is logged per outage; valid readings restore the valve. Status and
metrics use the last admission sample and never read procfs themselves.

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

Admin status and existing scheduling messages report CPU/IO observations and
the holding resource (`cpu`, `io` or `cpu+io`). Scoped and non-admin views retain
the actionable reason while withholding host-wide counts and measurements.
No statuses or layouts are added.

Authenticated admin metrics retain the pressure avg10/avg60 samples, availability,
enforcement/holding state and deferral/bypass counters. CPU full PSI is undefined
at host scope and reported as zero. Memory and full averages remain diagnostic;
they do not control this policy. No run/repository metric labels are added.
Counters reset on restart. See [admission and native synchronization](native-synchronization.md)
for the controls that remain.
