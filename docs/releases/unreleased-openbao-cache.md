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

The release retains its existing 600-second compile budget, 791-second total
deadline and 120-second test window. Memory requests/limits remain 3200/4096 MiB,
and storage requests/limits remain 8 GiB. These release-base bounds are retained
while enabling the cache changes; remeasure on CI, including Argo helpers,
before reducing them.
