# Authenticated native execution targets

One Oberth control plane continues to own Git ingress, runs, admission, grants,
logs, results and publication. `--argo-execution-target-config` optionally loads
one administrator-owned branch target. Without this file, new runs use the
existing local engine. In Helm, `argo.executionTargetConfigMap` names an
existing ConfigMap whose `config.json` key is this policy, mounted read-only at
`/etc/oberth/execution-target`; the optional `argo.executionTargetCredentialsSecret`
mounts an existing Secret read-only at `/etc/oberth/execution-target-credentials`
for the policy's absolute `kubeconfig` path. Previously bound remote runs still require their original
bundle after restart; removing configuration never redirects them locally.

The JSON object has schema `oberth.execution-target/v1`, a nonempty `id` other
than `local`, an absolute `kubeconfig` path, an explicit `context`, the complete
public Argo executor `config`. Credentials remain in the selected runtime transport, outside the stored
configuration and workload objects. The same authenticated transport constructs
Workflow, Kubernetes, logs, exec, dynamic-resource and cleanup clients. There is
no second scheduler or external success-result API.

Reboot attribution uses read-only `get` on the target's `nodes`, through this
same authenticated and cluster-pinned transport. Grant it to the target server
identity to retain boot IDs for owned running step Pods. It requires no node
watch, node proxy, kubelet endpoint or write permission. Missing permissions
or a missing pre-reboot observation leave the failure unclassified as a reboot.

There are no CPU capacity reads, pools or admission credits. Node reads above
serve reboot attribution, not placement or resource fitting.

`config.ExecutionPlacement` must use the closed
`oberth.execution-placement/branch-v2` form. It retains the version-one cluster,
endpoint/CA, controller, admission, source and native configuration pins.
It carries no node/location/taint selection. The removed `node_name`, `node_uid`,
`provider_id`, `zone`, `pool`, `taint_key` and `taint_value` fields are refused,
even when null. Upgrade removes those fields from stored executor snapshots;
original admitted objects, POST intents, UIDs and custody hashes remain bound.
The retained `node_version` is an operator admission-evidence value, not an
observation of a chosen worker or a placement guarantee.
It adds `branch_policy_baseline`, `graph_digest`, `fragment_lock_digest` and a
`fragments` inventory containing each exact `key`, `commit` and raw `digest`.
`normal` contains only `run_id` and `workflow`. No `preflight` or trusted-intent
field is accepted. The retired protected execution schema is refused.

This policy authorizes one exact ordinary branch execution. The operator must
know the canonical accepted run ID and its deterministic Workflow name before
admission, supply all reviewed target identities, and retain the original
bundle for recovery. The server independently resolves candidate and current
default trusted policy through canonical Git. Unknown/unreadable policy or a
real trusted requirement refuses this branch-only route. Fragments still pass
native loading, full content authentication, resolution and admission.

Oberth submits exactly the authored nodeSelector, affinity and tolerations; an
approved target neither injects them nor refuses different authored placement.
A dedicated node is guarded by Kubernetes taints plus the admission or namespace
policy the operator sets up. Repository YAML must carry the desired nodeSelector
and tolerations. Oberth does not guarantee that a target's runs land only on a
particular node, or isolate a trusted node from ordinary repositories: ordinary
repositories may author tolerations too. The former injected selector and node
incarnation checks constrained target work to a particular worker; that property
is removed. No replacement node-isolation mechanism is installed.

What a target still guarantees is authenticated routing to the approved
cluster/endpoint and namespace, the approved controller and service identity,
the exact authorized repository/ref/SHA/run/source/fragment scope, and canonical
original-UID custody for observation and cleanup. These guarantees do not prove
worker-node exclusivity, physical host isolation or available resources.

Before allocation, SQLite binds the canonical run to its target, defaulted
executor configuration, raw source/fragment bytes, graph/lock identities and
original cleanup names. Admission records the actual native Workflow and grants.
Every possible server POST first records its intent and bounded late-commit
horizon. Repeating an intent after a lost response permits observation of that
original object, never another POST. Original UIDs fence adoption and deletion.

Cancellation and uncertain source-PVC, seed-Pod, identity-Pod or Workflow outcomes
use the canonical pending-create reconciler. Terminal resource disposal shares
that reconciler and retains immutable original-UID cleanup receipts. Denied
reads, replacement UIDs, missing bundles and changed endpoint/cluster identity
retain custody. Accepted DELETE alone does not establish absence. Admission
expiry cannot authorize new work; observation and fenced disposal retain the
original cluster anchor and object identities.

The local controller's health remains `controller_profile`. Target controller
profiles are reported separately in `execution_target_profiles`; run admission
waits for its selected controller. Canonical run lifecycle and capability
authorization remain authoritative; Argo owns native queue, priority, locks and
authored deadlines, and Kubernetes owns Pod placement. Deployment still requires actual
target capacity/storage/device and transport qualification, containing source
review and CI, and operator approval. Offline fake-client tests establish no
installed route or physical execution result.
