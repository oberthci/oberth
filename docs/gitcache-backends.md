# Git cache backends and qualification

`go-git` performs transport and plumbing in-process; `cli` retains the Git/
OpenSSH adapter for the one-release transition. Both use the same receive,
release-admission, durable outbox and conservative promotion-merge policies.
The default is `go-git` in `serve`, `repo verify`, and the Helm chart's
`gitBackend` value. For one release, `--git-backend=cli` selects the legacy
adapter. The normal scratch image contains no Git/OpenSSH executables; using
that switch requires a separately built `Dockerfile --target cli-runtime`
image (or an operator-provided Git/OpenSSH installation). It does not install
tools into the scratch image. Configure both the compatibility image digest
and `gitBackend: cli` when testing rollback.

The fixture default is also `go-git`. CI has a binding
`test-gitcache-backends` lane which runs both adapters before any binary build;
`make test` runs the ordinary default-backend suite and the CLI cache suite.
Run all existing fixtures against both with:

```sh
./hack/test-gitcache-backends.sh
```

The Go backend reads only explicit SSH identity paths and pinned known_hosts.
It never consults an agent, ambient SSH configuration or a trust-on-first-use
store. Host discovery is separate and requires the existing fingerprint approval.

Local transfers copy independent object files, verify their checksums, then
validate the selected graph before committing refs. They create neither
hardlinks nor alternates. Objects stream lazily rather than accumulating all
blob contents in an encoding plan; each operation has a 32 MiB object cache.

Automatic maintenance starts at 64 packs or 1,024 loose objects. It makes at
most one attempt per 14-day retention window (or twice the configured command
timeout, if longer), limiting duplicate packs while old readers/run objects
remain protected. A failure is logged; explicit `Cache.Compact` can retry.
Compaction uses go-git's pack encoder in batches targeting 64 MiB of object
content and 8,192 objects; an oversized object occupies its own batch. A
one-object delta search window limits pure-Go redelta CPU. All new packs must
close successfully before any aged source pack or reachable loose object is
removed. Unreachable objects retain the same minimum 14-day grace window.
This trades some compression for bounded plans and CPU; measure real-mirror
storage before cutover. Recent source packs remain in addition to the new
packs during the grace window, so reserve space for both.

## Reproduce memory and time measurements

The source mirror is read-only. The script refuses an existing results
directory and writes its own two cache copies, detached workspaces and logs.
Allow several mirror sizes of free space. `--compact` measures explicit full
compaction in those disposable caches, never in the source mirror.

```sh
./hack/benchmark-gitcache.sh --docker --compact /path/to/cloudtaser-ebpf.git /tmp/bench-ebpf
./hack/benchmark-gitcache.sh --docker --compact /path/to/cttv.git /tmp/bench-cttv
# Reproducible laptop fixture: 60k commits, 80k blobs, 60k trees (~1.5 GiB pack).
./hack/benchmark-gitcache.sh --docker --compact --synthetic /tmp/bench-synthetic
```

Docker mode uses the repository's pinned Go 1.26.10/Git tool image, no network,
read-only source mount, four CPUs and an 8 GiB limit. Override `BENCH_CPUS` and
`BENCH_MEMORY` to repeat with the intended pod budget. Without `--docker`, the
host needs Go, Git and Python 3.9+ for fixture generation. The benchmark binary
can also repeat `ensure`, `checkout`, `gc` or `compact` on an existing dedicated
benchmark cache after its source mirror advances.

Each JSON result records elapsed time, process peak RSS, largest child RSS,
sampled Go heap, cache-directory bytes and runtime/platform details. Docker
mode additionally records whole-cgroup peak memory and memory.stat. RSS values
are not additive, and cgroup memory includes file cache; neither a parent's RSS
nor a child's maximum alone describes total memory. Fresh containers isolate
cgroup peaks per operation. File-cache ownership/reclamation and host filesystem
cache state still affect the numbers; these are single runs, not statistical
latency guarantees. Synthetic random blobs reproduce byte/object scale, not
the real mirrors' compression or blob-size distribution. Qualify both real
mirrors on tuxbox before production cutover.

## Scratch release substrate

The final Dockerfile target is `runtime`, based on `scratch`. Its only files
are the source-built static server and a CA bundle copied from the pinned
build stage. Go embeds the timezone database with `time/tzdata`. The runtime
uses numeric UID/GID 65534, `/data` as HOME/work directory and `/tmp` for
temporary work. The chart supplies writable `/data` and `/tmp` mounts and a
read-only root filesystem; standalone containers need the same writable
mounts, with `/data` owned or group-writable by 65534.

The release repacker retains the digest-pinned multi-platform substrate
mechanism, replacing its bootstrap executable with the exact release build.
Both assembly and readback enforce a positive path allowlist: the binary,
CA bundle and their directories, plus the two mount directories. An Alpine
substrate, OS package database, shell, Git, SSH or shared library is rejected.
Changing runtime/CA inputs requires rebuilding both platform children,
checking their static ELF architecture, scanning substrate and repacked
images, and publishing the exact reviewed OCI index with preserved digests
before updating/releasing its `ServerSubstrate` reference. The CLI target is
never a release substrate.

Receive-pack unpacks into a private object directory. Graph/header checks and
all per-ref policy/CAS prechecks run before promotion into the live object
store. The temporary alternate for thin-pack bases is read-only and never
copied into the live store. Rejected-only receives discard their object files;
accepted receives promote immutable objects before atomic ref publication.
Validation checks canonical tree order, raw commit/tag headers, portable .git
aliases and .gitmodules name/path/URL/update fields. Merge/archive traversal
propagates every missing-subtree error instead of accepting a partial walk.

Interrupted receive quarantines (`objects/oberth-incoming-*`) are swept on
Ensure after 30 minutes. A kernel lock held for the whole receive protects
active directories regardless of age, including across server processes;
crash exit releases the lock. Recent directories and symlinks are not swept.
