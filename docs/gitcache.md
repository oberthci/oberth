# Native Git source transport

Incoming Git SSH authentication reads key registrations and scopes directly
from the live SQLite WAL through a separate pool of at most four read-only
connections. Writer transactions, including audit verification, do not queue
these reads behind the server's single writer connection. Each lookup uses a
fresh snapshot; committed revocations and scope changes take effect on the
next lookup, including a second check after the client signs its key proof.
Existing authenticated connections retain their session identity and scope.
The daemon opens the reader pool before serving; CLI and one-shot maintenance
store handles do not open it.

A missing or revoked key receives the usual authentication refusal. A lookup
timeout or database failure instead logs `SSH identity lookup unavailable`
once per connection, sends
`temporary server error: SSH identity lookup unavailable; retry later`,
and closes the connection. Retry those connections; a server-side lookup
failure does not indicate that the client key needs to be registered again.

Git CLI is the only backend. There is no in-process Git implementation,
comparison worker or shadow telemetry. Retired `--git-backend`,
`--gitcache-shadow` and OBERTH_GITCACHE_SHADOW settings have no runtime role.
For upgrade/install/restore value reuse, the chart accepts and ignores exactly
`gitBackend: cli` and `gitcache.shadow: false`. `go-git`, newly supplied
`shadow: true` and unknown fields remain refused.
`oberth upgrade` migrates an already stored boolean `gitcache.shadow` by
removing that key from a private snapshot used for both schema preflight and
application, with an explicit diagnostic. All other user configuration remains
subject to the target schema; the original Helm revision remains untouched
for exact rollback. This migration does not enable a shadow worker.

The scratch server image contains static Git 2.56.0 (zlib only), static Oberth,
`oberth-git-ssh -> oberth`, and the pinned Alpine CA bundle. It has no shell,
OpenSSH, curl or package database. HTTPS upstreams are refused during validation and registration with a message
to use pinned-key SSH. `GIT_ALLOW_PROTOCOL=ssh:file` is forced even after
per-command environment overlays. Hooks requiring interpreters are unavailable.
Before upgrading an existing installation, capture its complete `oberth upstream
list` metadata and run the new CLI's `oberth preflight upstreams --inventory FILE`.
It refuses existing HTTPS registrations and reports names, never URLs or keys;
migrate them to verified pinned-key SSH before upgrading. Upstream source access uses SSH with key-file
authentication and the existing operator-pinned known_hosts; missing or
mismatched pins refuse before authentication. Registration's in-Go key probe
still requires operator verification; there is no TOFU in the Git transport.

Git invokes the native helper using GIT_SSH and GIT_SSH_VARIANT=simple. Its
closed grammar is `[-p port] host command`. Config/key/known_hosts reads are
bounded; ambient SSH config, agent auth, Include, Match and ProxyCommand are
not supported. Repository service commands execute on the remote forge, while
local transport dispatches built-in Git services directly without `sh -c`.
`hack/static-git-no-shell.patch` contains both reviewed upstream deltas:
simple passes explicit ports as `-p`, and local service argv is tokenized
without shell evaluation. There is no GIT_SSH_COMMAND or local shell fallback.

Git commands retain their process-group cancellation and descendant reaper.
Native SSH uses context-bound connections, closes streams on cancellation,
and preserves the remote exit status. On Linux its helper also dies with its
Git parent. The runtime stays non-root without additional capabilities/RBAC.
Cache preparation and restore file operations use the separately pinned
utility image; server execs use native Oberth subcommands.

`hack/test-gitcache.sh` gates the complete race-enabled CLI suite. The public
multi-pack history gate retains full-commit archive coverage and the checkout
regression; go-git-specific decoder and comparison tests were removed together
with that implementation. The required `build-static-git-runtime` / `test-static-git-runtime` CI chain
builds the same pinned C recipe as Dockerfile's git-build stage, then runs the
complete tagged CLI suite on the already allowed, digest-pinned Go runner.
The built scratch-like payload is mounted read-only; Oberth's verified nonroot
profile assigns real UID/GID 65534. A static, single-threaded CI launcher uses
Landlock to expose only payload/fixture/tmp/proc/dev reads and permit execution
only of candidate Git, Oberth and the test executable. The runner's templates
and interpreters cannot mask missing runtime dependencies; PATH has no shell.
Git object/quarantine moves remain allowed within non-executable `/tmp`.
Landlock ABI 2 or newer and the already enabled `nonroot-static-v1` controller
profile are required: unavailable restrictions fail closed. No image-prefix,
privilege, capability or security-context exception is introduced. The full
suite retains early upload/receive exit128, mismatch, fetch and cancellation
checks, plus explicit UID/capability/read-only/no-shell assertions. Native-amd64
behaviour is qualified; arm64 retains static/byte/scan checks.

The receive hooks are symlinks to the running Oberth executable. Native
pre-receive/update dispatch preserves batch/public-ref bounds, duplicate
refusal, deletion recovery, default-branch ancestry, immutable tags and fresh
upstream release admission. Missing template hook directories are created;
existing scripts are replaced atomically and symlink directories are refused.
Client hidden-ref refusal stays in the Git receive configuration, while
trusted local promotion writes retain private-ref persistence.

Rebuild from the pinned Dockerfile: Git 2.56.0's official source archive is
`https://www.kernel.org/pub/software/scm/git/git-2.56.0.tar.xz`, SHA-256
`26c56c296b38c0695b26fa95f475f1d01704d2d38e73465ca30b0b2f5dc789d3`.
The source build disables curl, OpenSSL, Expat, Perl, Python, Tcl/Tk, gettext,
iconv, PCRE2, system regex and Rust. Git's C implementations, musl and zlib
are linked statically. The source patch is applied with zero fuzz. Both
platforms must be rebuilt and their exact index published before a release.

Standalone `serve` and `admin gc` installations also need an adjacent
`oberth-git-ssh -> oberth` executable alias. The container ships that alias;
`oberth git-ssh` remains available as an explicit subcommand for diagnostics.

The exact build APK closure, source/patch/scanner hashes and linked musl/zlib
revisions are in `internal/staticgit/build.env`. Qualified Git and linked archive
hashes are architecture-bound. A CycloneDX SBOM records source Git as generic +
CPE (not a falsely Alpine-patched Git), plus actual Alpine musl and zlib. The
release helper verifies the Git bytes in each final image before the Grype C
scan and existing Trivy Go scan. A known-vulnerable source-Git control must
produce HIGH/CRITICAL findings; the actual components must have none, including
ignored findings. Missing metadata, drift, scan errors and stale databases
refuse signing. Image labels expose the component versions and source hash.

Alpine's live repositories may retire pinned APK revisions. Rebuilds then fail
closed even for unrelated source changes. Follow the [static Git re-pin and
re-qualification procedure](../internal/staticgit/README.md); preserve the
required `test-static-git-runtime` gate and review both architecture byte pins
before accepting a replacement substrate.
