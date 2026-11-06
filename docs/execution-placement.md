# Isolated branch execution placement

`serve --argo-execution-placement-config=/etc/oberth/execution-placement/config.json`
selects an administrator configuration for isolated ordinary branch workflows.
The chart mounts an existing public ConfigMap with
`argo.executionPlacementConfigMap`, using its `config.json` key. The daemon reads
it once. Configuration, signing/custody records and executor deployment belong
to the operator; repository identities must not be able to write them.

Only `oberth.execution-placement/branch-v2` is supported. Unknown or duplicate
fields, case variants, missing identities and expired admission authority refuse.
The retired protected execution schema cannot authorize work. No repository
request can provide this configuration; there are no wildcard targets or inferred
profiles. Empty configuration preserves normal local execution.

The record binds the exact repository, durable repository ID, ref, SHA,
`.oberth/build.yaml` source digest and `normal` run/workflow identity. It also
binds `branch_policy_baseline`, resolved `graph_digest`, `fragment_lock_digest`
and each fragment's key, commit and digest. Native policy authority proves the
retired test contract is absent at both the candidate and fresh upstream default.

The operator also pins:

- The namespace, pipeline ServiceAccount, node name/UID, provider, zone, pool,
  taint and complete kubelet version.
- Cluster creation time and anchor UID, authenticated API endpoint/CA identity,
  control-plane version and immutable admission configuration.
- Controller Pod, ReplicaSet, Deployment and configuration names and UIDs.
- Running native binary, dependency configuration and admission-policy digests,
  plus an expiry time.

Source delivery and Workflow generation check the same selection. Templates
receive the fixed selectors and tolerations. Nonroot controller proof, readonly
source, private scratch, helper budgets and ordinary resource admission remain
mandatory. No device capability is inferred from placement. Operator resource
and extended-resource grants are independent.

The canonical target journal records the selected cluster, exact source capsule,
created object UIDs and observations. Recovery uses the retained target snapshot
and authenticated transport for that target only; it never falls back to local
execution or a different cluster. Admission expiry blocks new creation without
removing observation and disposal authority over existing bound objects.

The configuration pins the native executable and dependencies. Reissue operator
admission configuration when those identities change. Retired runtime fields are
not accepted as authority. Empty compatibility slots remain solely in internal
branch identity hashing so existing branch placement digests keep their layout.
