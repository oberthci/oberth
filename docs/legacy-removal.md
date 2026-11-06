# Finishing the execution-engine retirement

The preparatory **release** (v0.17.84)
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
A retained multi-document YAML artifact is inspected document by document;
legacy declarations or malformed later documents refuse the whole file, and
fragment references are collected across all documents. This is a source census,
not permission to execute the artifact: workflow and protected-contract decoders
still require one document. Each file is bounded to 1 MiB, 256 documents and
64 levels of nesting; exceeding a bound refuses rather than inspecting a prefix.
It names the repository, ref, commit and file that still declares KVM injection,
guest-test injection or trusted test producers. A retained `.oberth/tests.yaml`
is checked against the installed generic protected executor and its current
approved recipes, positive KVM group and exact-namespace device capability;
legacy/recovery-only configuration or missing authority refuses it. Keep the
contract and its independent suite/guest/harness requirement. See
[retained independent test contracts](retirement-upgrade.md#retained-independent-test-contracts).
Legacy KubeVirt resources/devices need a server-owned operator capability; repository
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
   workflows/operator capabilities while preserving independent protected test
   contracts and receipt/publication gates. Qualify the complete replacement
   runtime and original-owned recovery before treating source compatibility as
   migration evidence. Change their workflow YAML in those repositories; no
   Oberth-specific repository exceptions or contract-deletion bypass are available.
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
