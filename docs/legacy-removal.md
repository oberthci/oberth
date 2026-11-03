# Finishing the execution-engine retirement

The preparatory **release** (the v0.17.84 input on `fix/ob-84-laptop`)
exposes `oberth admin retirement-check` through the live daemon's private,
peer-authorized admin socket. It retains the old execution and recovery engine.
The final removal (`03728183` from the original series) is excluded and must
ship in a **later release**, after the preparatory release has been installed
and the live migration/drain evidence below has been collected. Publishing both
in one release leaves older installations without a proof-capable intermediate
release and is not supported.

The inventory is an on-demand administrator operation. It never gates server
startup: upstream outages, archived repositories and newly pushed legacy branches
can refuse a requested verification, but cannot prevent the listeners starting.
A later final-removal implementation must retain that availability boundary.
Upgrade preflight may require a successful live check; there is no override flag.

The check covers every registered repository, fresh upstream branch heads and
unpublished branches still held by Oberth. It reads all regular YAML sources
under `.oberth` and follows the exact pinned fragments used by those sources.
It names the repository, ref, commit and file that still declares KVM injection,
guest-test injection, trusted test producers or `.oberth/tests.yaml`. Legacy
KubeVirt resources/devices need a server-owned operator capability; repository
annotations cannot assert one. Unreadable or stale history, malformed YAML,
symlinks/submodules, unsupported fragment nesting and exceeded inventory limits
refuse the check. Historical tags alone do not block retirement; referenced
fragment tags do. New attempts to run obsolete history remain subject to the
new engine's normal admission refusal.

Archived mappings are still registered: migrate them before archiving, or remove
their mapping through the normal audited repository lifecycle after preserving
their history. Archiving is not proof that a declaration has migrated.

The daemon also requires an empty queue and no running runs, unfinished
cancellations, unclean VM/suite journals or capacity slots. It never deletes or
marks these records cleaned. The existing Kubernetes inventory guard separately
requires all externally owned legacy Workflows and VM/VMIs to be absent.

1. Publish and install the preparatory release while retaining the recovery engine.
   The final removal must not be the first installed binary to expose the check;
   the upgrade CLI refuses an old daemon with no proof protocol.
2. Migrate the dependent repositories and their pinned fragments to ordinary
   workflows/operator capabilities. Change their YAML in those repositories;
   no Oberth-specific repository exceptions are available.
3. Stop submissions and scheduled triggers at their source. Drain with the
   existing owner; keep its grants and cleanup RBAC until resources disappear.
4. Run `oberth admin retirement-check` inside the current Oberth Pod. The command
   performs no application mutation. Its success output is
   `{"format":"oberth-retirement-check-v1","ready":true}`; failures name what
   still needs migration or cleanup. Inventory is bounded to two minutes,
   1,000 repositories, 1,024 refs per repository, 256 YAML files per tree and
   16 MiB of distinct source bodies per repository. Narrow oversized histories
   through the ordinary repository lifecycle rather than accepting a prefix.
5. Take the normal verified backup. Publish and install a later final-removal
   release only after its separate review and live acceptance. The CLI must
   recheck before replacing the proof-capable preparatory daemon. Keep sources
   stopped throughout the cutover; the observations are not a durable submission
   pause. Resume submissions after readiness.

Direct Helm/image replacement bypasses the CLI preflight and is not the staged
upgrade procedure. Keep the preparatory release and verified backup for rollback.
Never edit the database or delete ownership records to bypass a refused check.
Legacy journal tables are historical evidence, not permission to mark external
resources deleted. Ordinary startup recovery remains responsible for its own
pending work and readiness; retirement inventory is never a process-start gate.
