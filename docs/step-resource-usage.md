# Step resource measurements

Counters are nullable, self-reported advisory telemetry. Build code can forge
its own termination message; never use these counters for admission or size
grants. Future aggregations must saturate on overflow. No aggregation is
currently performed. CPU durations are nanoseconds (divide by 1e9 for seconds).

The server's single-architecture executable is streamed and SHA-256 verified
once into each ordinary source PVC. Wrapped container/script Pod templates
add `kubernetes.io/arch` matching the server's `runtime.GOARCH` through
`podSpecPatch` strategic merge, preserving administrator workflow/parent selectors. Protected
KVM workflows and unsupported leaves remain unwrapped. This adds no hostPath,
capabilities, ServiceAccount token or RBAC.

Instrumentation runs in the main container's cgroup and consumes its budget.
The current wrapper is the full server executable: all linked Go package
initializers run before `internal-step-exec` dispatch. It is not a small shell
wrapper. New container/script leaves are therefore measured only with an
**explicit container memory limit of at least 256Mi**. Smaller or unspecified
limits keep their original command, resources and scheduling constraints;
usage remains null. A request is not a limit, and inherited/defaulted limits
are not sufficient evidence of headroom. Oberth neither raises a limit nor
adds a memory request. This cutoff is an eligibility policy, not a guarantee
that the remaining budget is sufficient for arbitrary user code.

At admission the server logs a warning naming the Workflow, template, limit
and instrumentation floor. The warning is also retained on the template as
`oberth.ci/step-usage-memory-warning` (visible on the Workflow and its Pod).
The server stamps `oberth.ci/step-usage-memory-policy` on new Workflows so
restart/adoption preserves both the new policy and already-running older
wrapped specs. Existing Pods are not rewritten: resubmit a failed tiny step.
Skipping usage instrumentation does not remove a repository's explicit
`oberth secretstore exec` or Argo's emissary; those still consume memory.

Run the admitted tiny-step command through the real Argo v4.0.8 emissary
under a 32Mi cgroup with swap disabled:

```sh
hack/test-step-usage-memory.py --argoexec /path/to/linux/argoexec \
  --receipt /path/to/receipt.json
```

Docker runs locally; the cluster gains no permissions. The receipt includes cgroup
peak/events, process RSS and container exit/OOM state. The short shell script
holds a 4MiB variable so the test exposes overhead that an empty `true` can
hide under reclaim. Supply `--baseline-binary` to compare the same command
forced through the previous full-server wrapper. The binary, emissary and
BusyBox image must match the selected Linux architecture; the default fixture
image is arm64. `--image`/`--platform` support other local targets. Missing
tools or a failed container are errors, not skips or successful evidence.

On 2026-10-28 a Linux/arm64 Go 1.26.10 release-flag build on v0.17.61
measured roughly 45–48MiB wrapper RSS without memory pressure, including
reclaimable executable pages. Argo plus the wrapper peaked at 139–141MiB in
a 256Mi cgroup; this motivated the 256Mi eligibility floor. The 32Mi short
script passed 10/10 admitted runs; the identical forced-wrapper runs were
OOMKilled/137 10/10. These are synthetic local measurements, not a capture
of a production Pod. Even an empty command can hide the regression because
Linux reclaims executable pages aggressively under pressure.

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
