# Finishing the execution-engine retirement

The preparatory commit exposes `oberth admin retirement-check` through the
live daemon's private, peer-authorized admin socket. It retains the old recovery
engine. The final removal commit enables the same check at startup and before
each upgrade's preparation and first live write. No override flag bypasses it.
After migration and drain, the check succeeds automatically; no further product
patch or deployment-specific repository allowlist is needed.

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

1. Deploy the preparatory commit series while retaining the recovery engine.
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
5. Take the normal verified backup, then upgrade to the final removal commit.
   The CLI rechecks; startup checks again before creating the execution engine
   or listeners. The observations are not a durable submission pause, so keep
   sources stopped throughout cutover. Restart submissions after readiness.

Direct Helm/image replacement bypasses the CLI preflight and is not the staged
upgrade procedure. A failed startup must not be worked around by editing the
database or deleting ownership records. Retain the preparatory recovery binary
and verified backup for recovery. Legacy journal tables are historical evidence,
not a request to silently mark old external resources deleted.
