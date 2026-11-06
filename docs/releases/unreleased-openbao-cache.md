# Faster OpenBao integration builds (#1489)

The OpenBao integration step reuses the repository Go module and build caches,
with shared per-run cache mounts as a fallback. Setup tools remain read-only.
The installer integration binary no longer includes race instrumentation; the
unit suites retain it. OpenBao itself is still a pinned release download checked
against its SHA-256, and all policy-isolation and setup/TLS tests still run.

Local Linux arm64 measurements with Go 1.26.10 took 97.6 seconds with an empty
build cache and 3.82–3.93 seconds on three warm builds. Modules were already
cached. The exact warm step, including download, verification and real OpenBao
tests, passed in 41.0 seconds; the full cold-build-cache step passed in 133.1
seconds. These are local measurements, not CI-under-load results.

The compile phase budget drops from 600 to 180 seconds; the total step deadline
drops from 791 to 371 seconds, retaining the full 120-second test window. The
memory request drops from 3136 to 2624 MiB, providing 20% headroom above the
measured full-step 2147.9 MiB cgroup peak, rounded upward. The memory limit keeps
the reviewed #1287 R1 failure floor of 4096 MiB; the storage limit remains 8 GiB.
Storage requests stay provisional because scratch measurements are sampled.
Remeasure on CI, including Argo helper usage, before reducing those bounds.
