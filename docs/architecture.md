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
name and SHA used by tag admission, so replay cannot silently switch ancestry
authority after an upstream default-branch rename.
Oberth records one durable run, marks any active run for the same branch
`interrupted` with supersession metadata, retains the Kubernetes cancellation
obligation, and places the replacement at the tail of a global queue. Claims
choose the oldest eligible run. A pending
publication gates only its exact repository, ref kind, and ref, preventing two
green pushes from publishing that ref concurrently while other refs continue.

When capacity is available, Oberth creates a detached workspace at the exact
accepted commit and submits an Argo Workflow from the repository's
`.oberth/build.yaml` (branch triggers) or `.oberth/release.yaml` (tag
triggers). Oberth sets metadata, forces the ServiceAccount identity by trigger
tier, and injects the source checkout into every container template. Each DAG
task runs in its own container with its own exit code and named log slice.
CPU, memory, and ephemeral storage have requests and limits;
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
after termination. Structured step results come from the runner's final
`oberth-summary` marker record in that verified log; the termination message
keeps only a minimal status for `kubectl describe`, with the legacy step-array
message still decoded from runs of older SDK versions. Byte ranges for every
`[burn/step]` section let one named-step MCP read return exactly that step
after Kubernetes reaps the pod. A run fails if retained output exceeds the
fixed 64 MiB ceiling.

The Argo controller starts following pod nodes as soon as they are known,
including Pending nodes whose containers can start before Argo publishes
Running. It follows each main container with Kubernetes
timestamps and serializes complete `[burn/step]` lines with progress records.
There are at most 32 followers per run; excess pods remain eligible for the
next observation and final drain. Retry attempts of one logical step drain in
order. Each attempt has a 32 MiB output ceiling and each timestamped record has a
1 MiB ceiling; all output and delivery
metadata share the run ceiling. A final non-following read reconciles the
inclusive timestamp boundary before delivery completes. Missing boundaries,
changed pod/container identities, unavailable logs and adoption without a
cursor produce explicit continuity diagnostics. Delivery failure alone leaves
the command verdict intact; retained writer failures and aggregate budget
breaches fail observation. `::oberth-log::` records carry delivery state separately
from command output. Active step readers build a snapshot index over the same
retained file, including interleaved ranges; browser disconnects have no effect
on the controller's followers.

Branch and tag triggers are distinct trust domains. Branch Jobs mount no Secret
and use `/var/cache/oberth/ci`. Before Git publishes a tag ref, a server-owned
receive policy rejects deletion, movement, upstream conflict, and any commit not
reachable from the fresh upstream default-branch snapshot. The Go backend
checks these rules before CAS ref updates; the compatibility backend uses a
server-owned hook. Admission resolves full symbolic `HEAD`, avoiding ambiguity
when a tag shares the branch's short name.
Release Jobs use
`/var/cache/oberth/release` and only the configured Secret names. Every mounted
Secret key is snapshotted at admission, freezing its bytes for the pod lifetime.
The immutable snapshot is projected as one read-only volume that preserves
`/secrets/<original-name>/<key>`. It is owned by the Job and garbage-collected
with it; the server can create snapshots but cannot read or delete arbitrary
namespace Secrets. Every snapshotted value is masked before retained logs are
written.

Plan and Apply are a third, explicitly authorized trust domain. Ordinary Test
and Build remain secretless. An actor authorizes Plan for one exact
terminal-green SHA only while it fast-forwards onto the exact current target
base. The repository declares a paired Plan/Apply pipeline, one
repository-scoped backend identity, bounded regular lock files, and distinct
operator-owned secret namespaces. Plan credentials live only below
`oberth/upstream/<org>/<repo>/plan/`; Apply credentials live only below the
matching `/apply/` subtree. Admission and pre-fetch snapshotting both enforce
the typed phase before store login, so repository code cannot borrow Apply
capability during planning. Present empty values survive as zero-byte files but
do not create an empty redaction rule. Writable module and build caches are
also split into distinct per-repository `plan` and `apply` trust tiers; neither
phase can persist executables or credential-derived state into the other's
cache.

The Plan runner writes only the fixed memory-backed path. A mandatory residue
gate retires escaped repository processes and listeners before server-owned
`save-plan` capture. The server rejects symlinks and artifacts over 16 MiB,
binds plaintext digest and the complete repository/source/target/run/tool/
lock/config/backend/actor identity, and encrypts the bytes through
verified-HTTPS OpenBao Transit before storing ciphertext in SQLite on the
Oberth `/data` PVC. Plaintext never reaches the PVC. Terminal transitions clear
the live ciphertext column while preserving immutable audit metadata; SQLite
may retain already-encrypted remnants in freed physical pages, so this is
logical live-storage clearing rather than a claim of physical erasure.

An Apply-capable promotion must attach one explicit ready plan while the target
still equals its bound base and the result equals its planned SHA. Divergence
requires a new green SHA and Plan. Oberth publishes Git first, records that
integration result independently, and only then enqueues one exact Apply under
the durable backend reservation. The Apply init container decrypts and verifies
the published binding into a memory `emptyDir`; the repository runner receives
that volume read-only and cannot replace the file or access a delivery socket.
There is no replan fallback or replay. Scheduler reconciliation recovers a
stranded post-publication enqueue after restart. Apply can fail after a passed
promotion, but that failure neither rolls Git back nor changes publication
evidence.

The Job substrate is the repository-declared standard golang image
(`var RunnerImage`, statically parsed and validated against an administrator
prefix allowlist; absent, derived from go.mod as golang:<version>-alpine). It
executes the repository's `.oberth` module directly with `go -C .oberth run .`
— there is no runner binary and no interpreter. Repository-owned setup steps
fetch and checksum every additional compiler, linter, scanner, chart tool, and
signer. Writable Go caches are isolated by repository and by CI/release trust
tier; executable tools always come from the checked-out repository's setup
definition.

Oberth's own tag pipeline repeats its source gates before publication. It builds
reproducible multi-platform server executables, creates a clean
GAR image whose only application executable is that exact build,
scans and signs their digest references, and performs complete registry
read-back. The server runtime starts from scratch: a static Go executable,
CA trust data, embedded timezone database, and numeric user 65534. Git
transport/plumbing and host-key probing run in Go with explicitly pinned
known_hosts; the legacy CLI adapter has a separate one-release compatibility
image. Each platform has a matching pinned scratch substrate; an amd64
executable is never relabeled as arm64. Its packaged Helm
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

The release Job never pre-publishes its admitted tag. Oberth's own repository
Release DAG holds no upstream-forge mutation credential and makes no forge
release or ref mutation; it only publishes and verifies immutable artifacts.
After the DAG is terminal green, the publication owner pushes the exact admitted
annotated tag object. Equality of peeled commits is insufficient: a forge release
API can synthesize a lightweight tag whose peel matches while its object identity
conflicts. Any human-facing forge release therefore happens only after Oberth's
tag publication, outside the in-band DAG.

Only default-branch and scheduled branch runs register CI issue work, receiving
a global admission sequence in the same transaction as the run. Ordinary
feature-branch runs, promotions, and tag/release runs register none. Per-branch
and per-origin watermarks keep projections monotonic: delayed older completions
cannot overwrite newer failures. Existing branch and promotion projection
records remain usable for recovery and reconciliation. Red creates or updates
one open issue per repository and branch; green closes it. Projection never
acquires, renews, or deletes issue locks. Each branch failure projection includes
the caller-supplied bounded retained-log tail and a command hint for the complete
failed-step log; the transient tail is not stored on the run record.
An upstream reconciliation closes stale branch CI issues after a merge into a
green default branch or a verified upstream deletion. It checks the accepted
local branch head for ancestry, since a failed newer push may be ahead of the
last published upstream ref. The close reason is retained in the issue and
audit history; default-branch issues still require their own green run.
Manual issues are workspace-global flat-list records; they are never attached to
a repository or synchronized to the upstream forge.

Promotion first green-gates the candidate. It reuses the already-tested commit
only when the fetched target fast-forwards to that exact candidate. A divergent
target is merged locally and receives a new CI run. Both Git backends share a
conservative tree merge: overlapping path changes require identical resulting
objects and modes; otherwise promotion refuses with a `rebase onto <target>`
hint. There is no content or rename inference; file/directory conflicts and
ambiguous merge bases also require a rebase. When the fetched target
already contains the candidate, that fetched target tree also receives a new CI
run. The final upstream push is never forced, so target movement produces a
terminal failed promotion rather than overwriting work.

Repository default branches are discovered from the upstream cache and updated
durably when the upstream changes. A direct default-branch receive follows the
same CI and green-publication path as every other branch. Explicit promotion is
available when an uplink wants Oberth to merge and prove a source against a
chosen target; its required audit is durable before its CI run becomes claimable.

The database retains an explicit schema identity and a strict migration ledger.
Fresh installations use `oberth-schema-v1`; existing deployments supply their
original identity through administrator-owned configuration, alongside the
original audit domain and witness key derivation context. These values remain
stable across upgrades so persisted schema checks, audit hashes, and signing
keys keep their original meaning. The schema builder can convert every populated
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

External anchoring is opt-in and off by default. `auditAnchor.tsaURL` and
`auditAnchor.rekorURL` both default to empty: a fresh install runs the
complete local hash chain — every action chained, verified at startup, on
every cycle, and before every mutation — without contacting any external
service, so an outage of a public free service can never block CI. Setting
`rekorURL` enables everything above that involves Rekor witnesses, intents,
and rollback-external continuity; setting `tsaURL` enables RFC 3161 signed
checkpoints and their freshness gate. Both together restore the full
externally witnessed, rollback-protected posture, against the public
instances or self-hosted ones (`rekorCA` / `tsaCA` pin private trust
anchors). What local-only mode gives up is exactly the external evidence: a
whole-database rollback to another self-consistent chain is not detectable
without a witness.

Readiness covers completed startup recovery, the database and configured upstream
identity. Kubernetes availability and periodic audit health are diagnostics in
`/api/status`; they do not withdraw an otherwise usable cached Git/SSH endpoint.
Every mutation still passes its audit integrity gate. Serving prerequisites
refresh a bounded background snapshot; the HTTP probe performs no network or
store calls. Shutdown
removes readiness before stopping admission and drains HTTP/SSE observers. See
[restart handover](restart-handover.md) for client retries and upgrade behavior.
Upstream availability is a bounded diagnostic in `/status`, not a readiness
dependency, so an upstream outage cannot induce a control-plane restart loop.
