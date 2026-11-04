# Host pressure admission

Oberth bounds concurrent workflows and uses a configurable hard host CPU/IO
pressure valve. Kubernetes places each step Pod by its requests; no declared
CPU/memory pool or size-weight budget controls workflow placement. Running and
adopted workflows keep their slots. This valve observes one shared host: on a
multi-node installation, the server's node may differ from the execution node.

```yaml
admission:
  pressure:
    enabled: true
    hardCpu: 50
    hardIo: 40
```

The active flags are `--admission-pressure-enabled`,
`--admission-pressure-hard-cpu` and `--admission-pressure-hard-io`. Disabling the
valve leaves the administrator's concurrent-workflow ceiling in control.
Historical soft thresholds, observation mode, max-wait/defer settings and avg60
settings remain accepted but are ignored by serving admission.

Admission reads bounded `/proc/pressure/cpu` and `/proc/pressure/io` samples.
Hard CPU and IO thresholds use **some avg10**, defaulting to 50% and 40%.
Each resource has an independent latch and recovers strictly below its hard
threshold minus ten percentage points. One recovering resource cannot clear
the other's latch. Hard pressure has no idle, release/tag, priority or starvation
bypass. There is no settle window or resource drain forecast.

PSI measures stall time, not CPU utilization or load average. These thresholds
need qualification against representative workloads and cannot guarantee a
load ceiling. Missing, malformed or unavailable PSI retains the existing
fail-open behavior: status reports `PSI unavailable; workflow ceiling admission`.
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
Counters reset on restart. See the [workflow admission upgrade note](declared-admission.md)
for the complete list of ignored settings and request defaults.
