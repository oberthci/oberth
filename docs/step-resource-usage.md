# Step resource measurements

Counters are nullable, self-reported advisory telemetry. Build code can forge
its own termination message; never use these counters for admission or size
grants. Future aggregations must saturate on overflow. No aggregation is
currently performed. CPU durations are nanoseconds (divide by 1e9 for seconds).

The server's single-architecture executable is streamed and SHA-256 verified
once into each ordinary source PVC. Wrapped container/script Pod templates
select `kubernetes.io/arch` matching the server's `runtime.GOARCH`. Protected
KVM workflows and unsupported leaves remain unwrapped. This adds no hostPath,
capabilities, ServiceAccount token or RBAC.

The wrapper forwards catchable signals to the child process group, except
SIGCHLD and Go runtime SIGURG, like emissary. SIGKILL and SIGSTOP cannot be
caught. Child signal death returns 128+signal (SIGSEGV is 139). A command that
cannot start returns shell-style 127, a failed step rather than emissary's
executor Error. An existing nonempty termination message written by the step
is preserved; usage is then unknown. A wrapper killed before it reports also
leaves usage null. See AGENT-CONTRACT.md for measurement scope and other gaps.

## Seed cost measurement

`hack/benchmark-step-seed.py BINARY NAMESPACE POD --samples 10` measures only
the binary delivery addition, with a disposable seed Pod mounting its writable
PVC at `/seed`. The container must be named `seed` (or pass `--container`) and
provide sh/cat/chmod/sha256sum. The script overwrites `/seed/oberth-bin/oberth`,
streams with SHA-256 calculation, then verifies through a second Pod exec,
matching SourceSeeder's commands. It also times two empty execs as a control.
It creates no cluster resources and changes no RBAC. Use an isolated Pod/PVC;
never point it at a running server or real source checkout.

On 2026-10-27, ten local kind v1.37.0 samples with the chart-pinned BusyBox 1.37,
a fresh local-path PVC, UID/GID 65534, no capabilities, a read-only root and a
1 CPU/128 MiB limit measured the 51,576,994-byte Linux/amd64 release-flag build:

| Operation | Median | Range |
| --- | --- | --- |
| Stream and hash | 0.315 s | 0.301–1.604 s |
| SHA-256 readback | 0.230 s | 0.219–0.241 s |
| Total binary seed addition | 0.552 s | 0.529–1.831 s |
| Two empty execs | 0.139 s | 0.127–0.150 s |
| Addition minus empty-exec control | 0.415 s | 0.380–1.694 s |

Every readback matched. Each sample removed the prior file before timing,
as production creates a new per-run file. OS/storage caches were warm after
the first sample. The amd64 binary was copied, not executed on the arm64 node.
These measurements include kubectl startup and the local API exec transport;
they exclude Pod scheduling, PVC provisioning and source archive delivery.
Production uses the existing Go exec client rather than kubectl processes;
network latency, remote PVC storage and CPU contention can change the cost.
Run the shipped harness from tuxbox's server network to measure that deployment.
