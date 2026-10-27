# Git cache backends and qualification

Release note (unreleased qualification): the default remains the Git CLI in the
server and chart. go-git is an explicit opt-in and now **fails closed** instead
of silently ignoring detected `export-ignore` / `export-subst` rules (including
macros, nested and symlinked `.gitattributes`, repository info/attributes, and
configured attributes files). Use the CLI backend for these repositories.

With `OBERTH_GITCACHE_SHADOW=1` (`gitcache.shadow: true`), CLI output is always
served. The observer still walks all go-git blobs, checks object integrity and
compares tree IDs when export rules exist. Only archive differences with a
detected rule and equal trees are `expected_attr_gap`; unused rules can yield a
clean sample. Deadlines are `timeout`; a missing `nice` is `nice_unavailable`
and skips the observer rather than running it at server priority. Shutdown
cancellation is excluded from all soak counters. Errors and expected attribute
gaps are not clean archive-parity coverage. No file contents enter warnings.

The fixture default is also `cli`. CI has a binding
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

## Release substrate

The runtime is the reviewed digest-pinned v0.17.56 Alpine substrate with Git
and OpenSSH, restored while go-git is requalified. The server remains static,
embeds timezone data, and runs as numeric UID/GID 65534. The chart supplies
writable data/tmp mounts and a read-only root filesystem. Go-git is an explicit
backend opt-in, not a scratch-image rollout. Changes to runtime package inputs
still require the Dockerfile SUBSTRATE REBUILD procedure and a reviewed index
pin in internal/releaseimage/image.go before release.

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
