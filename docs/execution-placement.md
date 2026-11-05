# Isolated execution placement

`serve --argo-execution-placement-config=/etc/oberth/execution-placement/config.json`
selects a closed administrator configuration for one isolated executor. The
chart mounts an existing public ConfigMap with
`argo.executionPlacementConfigMap`; its required key is `config.json`. Empty
configuration preserves existing behavior. The daemon reads the file once.
Changing its contents requires a restart and a new containing source review.
The ConfigMap, signing/custody records and executor deployment belong to the
operator. Repository and pipeline identities must have no write access to them.

The schema is `oberth.execution-placement/v1`. Unknown fields, duplicate fields,
case variants, multiple documents, missing identities and expired authority
refuse. No repository document or execution request can carry this configuration.
There are no wildcards, profile fallbacks or observed-Pod role selection.

The flat JSON fields are:

| Fields | Meaning |
| --- | --- |
| `repository`, `repository_id`, `ref`, `sha`, `source_digest`, `pipeline_file` | Exact canonical upstream/org/repository, durable ID, stored ref, 40-hex source commit, SHA-256 of original pipeline bytes, and `.oberth/build.yaml` |
| `namespace`, `service_account` | Exact pipeline namespace and credential-free pipeline KSA |
| `intent_digest`, `policy_digest` | SHA-256 identities of original durable allocation intent and reviewed policy |
| `cluster_id`, `cluster_created`, `cluster_anchor_uid`, `endpoint_identity` | Original cluster ID/createTime, authenticated kube-system namespace UID, and transport identity |
| `node_name`, `node_uid`, `provider_id`, `zone`, `pool`, `node_version` | Approved node incarnation, full kubelet build and exact placement |
| `taint_key`, `taint_value` | One keyed Equal NoSchedule entry, with nil seconds |
| `native_identity`, `dependency_identity` | Running executable SHA-256 and exact executor dependency/config identity |
| `controller_pod`, `controller_uid`, `controller_config_uid`, `replica_set`, `replica_set_uid`, `deployment`, `deployment_uid` | Independently read controller provenance; owner names and UIDs both match |
| `admission_config_map`, `admission_uid`, `admission_digest`, `admission_profile`, `control_plane_version`, `admission_configuration` | Exact immutable operator admission record, mapped profile, full control-plane build and reviewed configuration evidence digest |
| `expires` | RFC3339 authority expiry |
| `normal`, `preflight` | Two distinct exact objects, each containing `run_id`, `workflow`, `trusted_intent`; normal uses an empty trusted intent, preflight uses the journaled KVM intent digest |

The canonical configuration digest is SHA-256 of the typed document's Go JSON
encoding. It is stamped into the Workflow spec identity. Endpoint identity is
SHA-256 of JSON `[host, serverName, caData]`, with CA bytes encoded as JSON base64.
TLS verification and an explicit CA are mandatory; impersonation refuses.
Dependency identity is SHA-256 of the complete defaulted `argojob.Config` Go
JSON encoding with `ExecutionPlacement` set to null. It includes proxy URL,
namespace/CA, source class/image, identity and device settings, caches, deadlines
and resource policy. The configured binary must match the running executable.
These identities bind configuration; they are not runtime execution receipts.

Before any seed claim/Pod creation, selection compares the durable run, source,
ref, repository, pipeline and trusted intent. Build performs the complete source
admission first. The seeder uses the same selection and refuses its older
name-only API while this profile is enabled. Controller/seeder profile mismatch
also refuses. The authenticated Kubernetes client reads the cluster anchor,
current control-plane build, Ready node UID/provider/build/device capacity,
pinned controller and immutable
admission record before seeding and during protected observations. Controller
provenance includes the real Pod → ReplicaSet → Deployment owner chain, exact
pinned Argo image/process/configuration checks and readiness. Replacement
requires fresh authority; labels and supplied owner references cannot establish
that chain on their own.

The immutable admission ConfigMap has exactly eight string keys: `cluster_id`,
`cluster_created`, `control_plane_version`, `node_version`,
`admission_configuration`, `profile`, `intent_digest`, `policy_digest`.
`admission_digest` hashes this data map's Go JSON encoding. Root must derive this
record from independently authenticated control-plane, namespace, plugin/flag
and webhook evidence and review the mapping before enabling a managed cluster.
The upstream reference fixtures cannot establish the managed implementation or
populate a real installation's record. An unknown or different profile refuses.

Every selected ordinary candidate and protected harness gets exact linux/amd64,
pool, zone and node hostname selectors, the one native NoSchedule toleration,
1Gi `var-run-argo`, and 64Mi `tmp-dir-argo`. Argo init, wait and token init use
requests 50m/64Mi and limits 500m/512Mi. These are provisional helper bounds,
not measured peaks. Main resources, source PVCs, commands, UID/security, KVM
injection, token separation and instrumentation retain their existing rules.
The direct seed Pod receives the same selectors/toleration and retains its
100m/64Mi requests and 500m/512Mi limits. Its create response and UID-bound
readback must preserve the complete submitted executable/resources/storage/
isolation before any source bytes are streamed.

Native templates and ordinary/controller create requests contain only the
native entry. Final admission uses one preselected reference chain:
`kube1357-dts300` adds keyed Exists not-ready/unreachable NoExecute entries with
300 seconds; `kube1357-dts300-ptr-empty` also adds keyed Exists memory-pressure
NoSchedule for the supported non-BestEffort expected resources. Canonical
comparison preserves all five fields and nil versus zero seconds, ignores list
order and rejects repeated key/effect identities. No additional-key waiver is
provided. The protected verifier retains exact expected-leaf selector equality
and all whole-Pod executable, helper, mount, image, resource and security checks.

The internal reference policy also distinguishes reviewed control Deployments,
DaemonSets and host-network DaemonSets. DaemonSet indefinite node-failure
entries and host-network additions cannot transfer to candidate, seed or
harness roles. This engine does not install infrastructure manifests or accept
Pod-provided roles. Infrastructure/system manifests, owner-chain/device
allocation enforcement, real managed admission, full image/dependency/egress
closure, capacity observations and disposal remain separately required before
execution. Local fake-client construction, actual upstream controller rendering
and pinned admission replay each prove their own boundary only.
