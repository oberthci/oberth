# Standalone deployment configuration

Fresh installations use the protocol identifiers `oberth-schema-v1`,
`oberth-audit-v1`, and `oberth-audit-witness-v1`. Charts are published at
`https://charts.oberth.ci` and binaries at
`https://releases.oberth.ci/oberth/`.

Before upgrading an existing deployment to 0.17.0, preserve its original
protocol identifiers in a reviewed, administrator-owned values file outside the
source checkout. Obtain them from the deployed version's verified source and
existing public configuration. Do not substitute fresh-install defaults: the
schema identity, audit hash domain, and witness key derivation context are
persistent contracts. No audit rewrite or key rotation is part of this upgrade.

The required fields are `compatibility.schemaIdentity`,
`compatibility.auditDomain`, and `compatibility.witnessKeyInfo`. These fields
contain public protocol configuration. Keep secrets in
the existing secret store. Review the normal `oberth upgrade --dry-run --values
/absolute/path/deployment-values.yaml` output before the upgrade. Missing reused
protocol fields fail rendering; a mismatched database identity or audit domain fails before writable startup.
Subsequent upgrades retain the explicitly configured values with Helm's
`--reuse-values` behavior.

## Run submission

Oberth accepts durable run records and submits eligible Workflows without a
queue-depth cap. Argo controls Workflow parallelism; Kubernetes places Pods by
authored requests and placement. See [native scheduling](native-scheduling.md).

## Infrastructure failures

Oberth does not automatically retry infrastructure failures. Authors configure
native Argo `retryStrategy`; manual `run_rerun` remains. A source
preparation failure happens before Workflow creation and needs manual retry.
See [infrastructure failures and retries](infrastructure-retries.md).
