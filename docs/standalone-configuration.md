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
