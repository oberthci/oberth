# Architecture

An authenticated SSH push updates a bare repository cache on the PVC. Oberth
creates an actor-bound, fsync-backed receive reservation before Git can publish
the refs. Committed branch and tag events are replayed idempotently from that
outbox into SQLite, including after a callback failure or process restart.
The SSH handshake resolves one public-key fingerprint and actor for the lifetime
of that connection. Receive callbacks carry that bound value directly; a later
resolver or session-table change cannot reattribute an in-flight push.
Receive ordering has its own per-repository lock; the Git object lock is
released before a durable callback runs, so branch and tag admission can inspect
the accepted object without deadlocking or allowing a later receive to overtake
the callback. The receive policy and upstream materializer enforce the same
public branch/tag grammar, and remove invalid refs left by an older cache before
snapshotting. Each reservation also binds the exact fresh upstream default-branch
name and SHA used by the tag hook, so replay cannot silently switch ancestry
authority after an upstream default-branch rename.
Oberth records one durable run, marks any active run for the same branch
`interrupted` with supersession metadata, retains the Kubernetes cancellation
obligation, and places the replacement at the tail of a global queue. Claims
choose the oldest eligible run. A pending
publication gates only its exact repository, ref kind, and ref, preventing two
green pushes from publishing that ref concurrently while other refs continue.

When capacity is available, Oberth creates a detached workspace at the exact
accepted commit and creates one `batch/v1` Job. The Job has no service-account
token, runs as UID/GID 65534, has bounded resources and an outer deadline, never
retries, and is reaped one hour after completion. The runner interprets and
validates Periapsis inside that Job, then executes each step sequentially in its
own process group. CPU, memory, and ephemeral storage have requests and limits;
the writable `/tmp` `emptyDir` cannot exceed the same ephemeral-storage limit.
The shared PVC has a fixed requested capacity, while the intentionally
persistent CI/release host caches remain separate node-provisioned paths.

The Job name is deterministic and persisted before Kubernetes creation. Create
is idempotent only for the identical intended Job. Graceful shutdown cancels and
joins active workers; owner startup turns every interrupted Job intent into a
durable deletion obligation before serving new work.

Workspace paths are deterministic database-owned resources. Run and promotion
workspaces remain while their owner, cancellation, or publication is active.
Terminalization happens before cleanup; startup recovers paginated terminal
owners and never scans or deletes an unrecognized directory.

The server watches Job state and streams masked logs to the PVC. A retained-log
destination failure cancels the Job, while a source transport reset never
deletes healthy compute. The stream reconnects by verifying and discarding the
already-retained raw prefix, then verifies one authoritative non-following log
after termination. Structured step results come from the runner's bounded
step-array termination message. Byte ranges for every `[burn/step]` section let
one named-step MCP read return exactly that step after Kubernetes reaps the pod.
A run fails if retained output exceeds the fixed 64 MiB ceiling.

Branch and tag triggers are distinct trust domains. Branch Jobs mount no Secret
and use `/var/cache/oberth/ci`. Before Git publishes a tag ref, a server-owned
receive hook rejects deletion, movement, upstream conflict, and any commit not
reachable from the fresh upstream default-branch snapshot. The hook reads full
symbolic `HEAD`, avoiding ambiguity when a tag shares the branch's short name.
Release Jobs use
`/var/cache/oberth/release` and only the configured Secret names. Every mounted
Secret key is snapshotted at admission, freezing its bytes for the pod lifetime.
The immutable snapshot is projected as one read-only volume that preserves
`/secrets/<original-name>/<key>`. It is owned by the Job and garbage-collected
with it; the server can create snapshots but cannot read or delete arbitrary
namespace Secrets. Every snapshotted value is masked before retained logs are
written.

The runner image supplies only `oberth-runner`, a shell, Git/SSH, TLS roots,
curl, and archive extraction. Repository-owned setup steps fetch and checksum
every compiler, linter, scanner, chart tool, and signer. Writable Go caches are
isolated by repository and by CI/release trust tier; executable tools always
come from the checked-out repository's setup definition.

Oberth's own tag pipeline repeats its source gates before publication. It builds
reproducible multi-platform server and runner executables, creates clean
linux/amd64 GAR images whose only application executable is that exact build,
scans and signs their digest references, and performs complete registry
read-back. A container platform is added only with a matching pinned userspace
substrate; an amd64 substrate is never relabeled as arm64. Its packaged Helm
chart contains those exact image digests. Binaries and the chart use immutable,
conditionally-created R2 objects; stable binary aliases, `latest/VERSION`, and
the classic Helm index advance only after public R2 and GAR verification and use
ETag compare-and-swap for concurrent finalizers.

Every green branch, tag, or promotion creates a pending publication row before
any publication-side network read or Git mutation. It names the exact ref and
result object. Ordinary green branches force-sync that result. Tag delivery
binds the observed missing ref, while promotion already owns an exact predecessor.
Publication finalization atomically terminalizes the publication, linked run,
promotion when present, and CI issue projection. Restart recovery compares only
the recorded ref states once at owner startup. Result present is delivered;
ordinary branches make one bounded force-sync attempt, while tag/promotion
predecessor rules remain fail-closed. There is no recurring publication loop.

CI issue work receives a global admission sequence in the same transaction as
its run or promotion. Per-branch and per-origin watermarks make projections
monotonic: delayed older completions cannot overwrite newer failures, and an
ordinary green run cannot close an unresolved promotion failure. A qualifying
published promotion can close it without creating a second issue. Each branch
failure projection includes the caller-supplied bounded retained-log tail and a
command hint for the complete failed-step log; the transient tail is not stored
on the run record.
Manual issues are workspace-global flat-list records; they are never attached to
a repository or synchronized to the upstream forge.

Promotion first green-gates the candidate. It reuses the already-tested commit
only when the fetched target fast-forwards to that exact candidate. A divergent
target is merged locally and receives a new CI run. When the fetched target
already contains the candidate, that fetched target tree also receives a new CI
run. The final upstream push is never forced, so target movement produces a
terminal failed promotion rather than overwriting work.

Repository default branches are discovered from the upstream cache and updated
durably when the upstream changes. A direct default-branch receive follows the
same CI and green-publication path as every other branch. Explicit promotion is
available when an uplink wants Oberth to merge and prove a source against a
chosen target; its required audit is durable before its CI run becomes claimable.

The database retains the immutable identity `cloudtaser-oberth-schema-v1` (a
compatibility-frozen legacy literal — renaming it would be a breaking schema
migration for every existing deployment) and a strict migration ledger. The schema builder can convert every populated
version-1 audit row into one gap-free SHA-256 chain inside a single immediate
transaction; a malformed or missing historical row fails the conversion. The
live daemon does not run that conversion: it copies the existing database and
WAL into a private read-only inspection snapshot and accepts only the exact
current schema. Snapshot verification leaves the source database, WAL, and
derived WAL index byte-exact. It opens the source writable only after complete
external continuity verification. A fresh genesis
is created only after complete immutable intent and completion lists prove empty,
and it is witnessed before listeners or application mutation paths start. Audit heads receive
signed timestamps and linked public Rekor witnesses. Before publishing a
witness, Oberth creates a deterministic immutable intent outside the SQLite
PVC. Startup recovers and verifies the complete witness history from exact
Rekor UUID pins; only the single uncompleted intent can require discovery, and
its deterministic certificate plus exact audit hash form an authenticated AND
query rather than a floodable public-identity scan. Intent and completed-witness
sequences are pinned as deterministic immutable ConfigMaps outside the SQLite
PVC. Initial loading pages those records without a fixed count lifetime;
steady-state mutation gates retain the canonical immutable prefix and exact-read
its tips. A pending intent keeps all new mutations closed. If an operation that
passed its gate earlier finishes after intent creation, recovery witnesses the
exact intended prefix first and then the newer suffix. The server's namespace
Role can create, get, and list continuity records but cannot update, patch, or
delete them, so a crash followed by a complete database rollback cannot erase a
Rekor side effect. Owner recovery, Git receives, scheduling, and mutating MCP
calls recheck the local chain, latest signed anchor, verified witness history,
and rollback-external continuity before changing state. A database with an unknown
migration or missing immutable identity is rejected before recovery mutates
anything.

Readiness covers the database, configured projected identity, and Kubernetes.
Upstream availability is a bounded diagnostic in `/status`, not a readiness
dependency, so an upstream outage cannot induce a control-plane restart loop.
