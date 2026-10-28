# Native Git source transport

Git CLI is the only backend. There is no in-process Git implementation,
comparison worker or shadow telemetry. Retired `--git-backend`,
`--gitcache-shadow` and OBERTH_GITCACHE_SHADOW settings have no runtime role.
The chart schema refuses retired `gitBackend` and `gitcache` keys. Export
installed values, remove these keys, and reconcile explicitly using the
existing upgrade migration procedure; do not change compatibility identities.
Unknown values remain refused rather than silently discarded.

The scratch server image contains static Git 2.56.0 (zlib only), static Oberth,
`oberth-git-ssh -> oberth`, and the pinned Alpine CA bundle. It has no shell,
OpenSSH, curl or package database. Git HTTP(S) transports and hooks requiring
interpreters are unavailable. Upstream source access uses SSH with key-file
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
with that implementation. Runtime container qualification must also run the
same CLI suite using the bundled Git and native fixtures.

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
