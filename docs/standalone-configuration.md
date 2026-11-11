# Standalone deployment configuration

Fresh installations use the protocol identifiers `oberth-schema-v1`,
`oberth-audit-v1`, and `oberth-audit-witness-v1`. The private Go module mirror is
disabled until its administrator supplies an owned namespace and repository
mapping. Charts are published at `https://charts.oberth.ci` and binaries at
`https://releases.oberth.ci/oberth/`.

Before upgrading an existing deployment to 0.17.0, preserve its original
protocol identifiers in a reviewed, administrator-owned values file outside the
source checkout. Obtain them from the deployed version's verified source and
existing public configuration. Do not substitute fresh-install defaults: the
schema identity, audit hash domain, and witness key derivation context are
persistent contracts. No audit rewrite or key rotation is part of this upgrade.

The required fields are `compatibility.schemaIdentity`,
`compatibility.auditDomain`, and `compatibility.witnessKeyInfo`. If the existing
mirror is enabled, retain its enablement through Helm’s `--reuse-values` and provide
`argo.goProxy.modulePrefix`, `argo.goProxy.repositoryPrefix`,
`argo.goProxy.upstream`, and `argo.goProxy.organization`. The latter two must
match the exact registered upstream name and organization; matching repository
suffixes in another upstream grant no access. An empty repository prefix is
valid. The complete configured module namespace fails closed for unknown
modules, while public modules retain normal checksum verification.

Do not include `argo.goProxy.enabled` or `argo.goProxy.port` in the dedicated
upgrade override file: enablement is reused from the existing installation and
the installer pins its port. Both keys remain protected by the upgrade command.

These fields contain public protocol and routing configuration. Keep secrets in
the existing secret store. Review the normal `oberth upgrade --dry-run --values
/absolute/path/deployment-values.yaml` output before the upgrade. Missing reused
protocol fields or an enabled mirror without its mapping fail rendering; a
mismatched database identity or audit domain fails before writable startup.
Subsequent upgrades retain the explicitly configured values with Helm's
`--reuse-values` behavior.

To enable a mirror for the first time, use reviewed values through `oberth
install --upgrade --values /absolute/path/deployment-values.yaml` or the normal
Helm installation flow. The dedicated upgrade command preserves existing mirror
enablement and does not accept changes to that protected value. For example, an
administrator-owned `go.example.test` namespace and registered `forge/example`
upstream can map `go.example.test/wire` to `sample-wire` using repository prefix
`sample-`; supplying the same suffix under a different upstream never qualifies
that repository.

## Queue depth cap

An unscoped administrator can call `queue_cap_config` with
`{"max_not_started":8}` to set the branch queue cap. The default is 0, which
disables the cap. This setting lives in the database and survives restart.
Use `system_status.queue_cap` to read `max_not_started`, `not_started`,
`deferred` and the current `reason`.

At the cap, branch pushes are accepted into Oberth's Git cache, with the reason
`branch run deferred: queue at N/M`; their CI runs are created later. Deferred
receives keep the exact commit and actor, and create runs in arrival order when
the count drops below the cap. The existing scheduler priority rules then apply
to those runs. Both unclaimed queued runs and claimed runs waiting to start
count toward the cap. Deferred receives have no run yet and are counted
separately. Tags/releases, promotions and schedules bypass the cap and can take
the count above it. Park refs continue to skip branch CI.

Set `maxNotStarted` to 0 to drain the backlog without a depth limit. The accepted
commit and durable receive journal stay local while run creation is deferred.
Automatic upstream publication follows successful CI through the existing
publication path, including the default branch's non-forced update. Explicit
park and sync operations retain their existing policy and default-branch guards.
A deferred SHA has no run yet; wait for run creation before using run-based
commands such as `sync`.

## Automatic infrastructure recovery

`autoRetryInfrastructureBudget` defaults to 2 in Helm; the server equivalent is
`--auto-retry-infrastructure-budget=2`. Failed steps, including infrastructure-classified
step failures, execution deadlines after any step started or passed, and any
failure whose `retry_hint` class is `code` are excluded. Eligible branch
infrastructure failures get up to that many fresh attempts per lineage, each at
the original admission position and priority. Set it to 0 in
administrator-owned upgrade values for manual-only recovery; at most 5. See [eligibility, audit records and
upgrade behavior](infrastructure-retries.md).
