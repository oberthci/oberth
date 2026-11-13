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
comparison background task or shadow telemetry. Retired `--git-backend`,
`--gitcache-shadow` and OBERTH_GITCACHE_SHADOW settings have no runtime role.
The chart values `gitBackend` and `gitcache` no longer exist; `oberth upgrade`
refuses stored ones until they are dropped with `--set gitBackend=null` and
`--set gitcache=null`.

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
Landlock to expose only payload, fixture, scratch, procfs and device reads and permit execution
only of candidate Git, Oberth and the test executable. The runner's templates
and interpreters cannot mask missing runtime dependencies; PATH has no shell.
Git object/quarantine moves remain allowed within non-executable private scratch.
Landlock ABI 2 or newer and the selected nonroot test leaf are required:
unavailable restrictions fail closed. No image-prefix,
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

## Upstream host keys and repository paths

`oberth upstream add` pins the forge's SSH host keys once: every key a
well-known forge publishes, only the key matching `--expected-host-fingerprint`,
or the keys an operator confirms with `yes`. Registration, `upstream list
--check` and the Git transport then offer the forge only the pinned key
types, so a forge with RSA, ECDSA and Ed25519 keys authenticates with an
Ed25519-only pin.

When a forge rotates its keys, or an earlier pin is incomplete, verify the new
fingerprints through a trusted channel and rerun the same command with
`--repin`. It first checks the registration (another URL or key choice under
the name is refused before anything is scanned or written), then rescans,
accepts keys under the same rules, and replaces only this endpoint's lines in
the `oberth-known-hosts` bundle. The interactive prompt lists the pins being
replaced beside the scanned keys, marked new or already pinned, and the write
is audited as `upstream.identity.repin.oberth-known-hosts`, apart from a first
pin. An identical registration is kept. An entry Oberth did not
write (hashed, wildcard or multi-host) is refused rather than edited.

Repositories resolve to `<base URL>/<repo>.git`. A plain `git-shell` forge
keeps them relative to the git user's home, so its base URL needs `/~/`:
`ssh://git@forge.example/~/acme`, not `ssh://git@forge.example/acme`.
`upstream add --check-repo <repo>` reads that existing repository's ref
advertisement (no objects) before registering, and `upstream list --check`
reads one registered repository per SSH upstream; both name the path the
forge refused and suggest the `/~/` form; an in-protocol `ERR` answer counts
as a refusal. The first clone's error carries
the same hint.

A forge repository may start empty. While its default branch does not exist
(an unborn HEAD, which protocol v0 forges never advertise), the cache keeps
its recorded default branch, `main` for a new repository, even after other
branches are synced there; the first promotion or default-branch publication
creates it upstream. A protocol v0 forge does not say which unborn branch its
HEAD names: if that is not the recorded default (a repository initialized on
`trunk` or `master`, say), Oberth still creates `main`, and the forge's HEAD
stays dangling, so a direct clone from the forge checks out nothing until its
default branch is set there to the branch Oberth created.

A default branch is read as unborn only while this cache has never seen one
upstream. A default branch that existed and then
disappears beside other refs was deleted: Oberth serves the cache stale with
"upstream default branch unknown", policy reads and release admission refuse,
and nothing recreates the branch without its policy. Restore it on the forge.

Clients pin Oberth's own SSH endpoint from `oberth status` (the
`ssh host key` fingerprint) or the `ssh_host_key` field of `system_status`,
both served over the CA-verified HTTPS API.
