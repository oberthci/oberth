# Operator-owned workflow capabilities

Oberth runs repository Workflows. Pipelines own any objects they create, their
cleanup, and their test gates. An administrator can grant one repository an
operator-created identity for namespaced resource templates or qualified
extended resources. Ordinary CI requires no such grant.

The operator creates the ServiceAccount in the configured execution namespace,
its least-privilege resource RBAC, Argo task-result permissions, admission
policies, and a ResourceQuota scoped to the workload PriorityClass. The server
needs permission to request tokens for that exact account. For new capability
workflows, operators provide their infrastructure and resource permissions. Kubernetes API discovery must identify every
allowed kind as namespaced; missing or cluster-scoped kinds are refused.

For example, the MCP `workflow_capability_grant` arguments are:

```json
{
  "repo": "upstream/org/repo",
  "capability": {
    "namespace": "pipelines",
    "service_account": "operator-workload",
    "resource_kinds": [
      {"api_version": "example.invalid/v1", "kind": "Task", "actions": ["create", "get", "delete"]}
    ],
    "extended_resources": {"example.invalid/accelerator": 2}
  }
}
```

Use the actual API installed in the execution namespace. Grants are bound to
the durable repository ID, audited, and administrator-only. `workflow_capability_get`
reads the grant; `workflow_capability_revoke` revokes it. Archived repositories
cannot receive new grants. No historical technology-specific grant is converted.

A resource step needs a named template and one inline YAML/JSON object with a
static API version, kind, name or generateName, and the execution namespace (or
an omitted namespace). The only dynamic manifest substitutions admitted are
`workflow.name`, `workflow.namespace`, and `workflow.uid`; namespace itself
must be static. Duplicate keys, multiple objects, List kinds, manifest files,
arbitrary flags and dynamic object envelopes are refused. Object specifications
are opaque to Oberth; the operator must constrain their fields with admission
policy as well as RBAC and quota. The allowlist supports `create`, `get`,
`delete`, and `apply`. Argo resource deletion also needs Kubernetes RBAC `list`
and `watch` on the target kind, in addition to `get` and `delete`; these are RBAC
verbs, not extra capability actions.

The server assigns the operator identity and its resource executor. It refuses
shared pipeline, executor, secret-store and per-repository secret identities.
Container, script and helper steps keep the tokenless pipeline identity. Only
the admitted resource template's argoexec containers receive the operator token;
any other container that mounts it causes token delivery to be refused.
Operator capability workflows cannot also consume secret-store or WIF grants.
Grant scope and identity are checked again before create, adoption and executor
token delivery. Revocation prevents new authorization; it does not delete
objects already created by a pipeline.

Extended-resource requests and limits must be equal positive integers. The
bounded graph accounts overlapping steps, fan-out, helpers and parallelism;
its peak must fit the repository capability grant. This is a maximum for each
Workflow, not a shared device reservation across runs. There is no server-wide
extended-resource capacity setting or ledger. Cross-run device capacity comes
from the device plugin and node allocatable resources: Kubernetes leaves Pods
pending until the requested devices are available. Unbounded graphs, missing
budgets or declarations cannot fall back to size admission for extended resources.

## Operator controls for resource templates

For virtual machine instances (VMIs) created through resource templates, RBAC
controls kinds and verbs, not VMI spec fields. Install an operator-owned ValidatingAdmissionPolicy with a
ValidatingAdmissionPolicyBinding using `validationActions: [Deny]` and
`failurePolicy: Fail` on the policy. Match the granted kind and CREATE/UPDATE
requests from the capability ServiceAccount. For example, this match condition
selects the account from the grant above:

```yaml
matchConditions:
  - name: capability-account
    expression: request.userInfo.username == 'system:serviceaccount:pipelines:operator-workload'
```

This is only the account selector, not a complete policy. The policy's validations
must allow only approved digest-pinned images and volume types, cap CPU and
memory, reject serviceAccount, secret and PVC volumes, and require an owner
reference to the creating Workflow. Use `{{workflow.name}}` and `{{workflow.uid}}`
for that reference in the resource manifest. Validate every permitted field
against the installed kind's schema; granting `create` does not enforce any of
these restrictions. See the [admission policy reference](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/).

Give these workloads a dedicated, operator-created PriorityClass and require it
in the admission policy. Confirm that the controller copies that class to its
Pods. A plain namespace quota would throttle unrelated CI in the shared namespace.
Instead, scope the Pod CPU/memory quota to the dedicated class. For example, with
an existing `resource-workload` PriorityClass:

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: resource-workload
  namespace: pipelines
spec:
  hard:
    pods: "4"
    requests.cpu: "4"
    requests.memory: 8Gi
    limits.cpu: "8"
    limits.memory: 16Gi
  scopeSelector:
    matchExpressions:
      - scopeName: PriorityClass
        operator: In
        values: [resource-workload]
```

Size this quota for all matching Pods, including controller overhead. It does
not reserve devices; a PriorityClass-scoped quota supports Pod count and ordinary
CPU, memory and ephemeral-storage resources, not extended-resource counters.
See [PriorityClass quota scope](https://kubernetes.io/docs/concepts/policy/resource-quotas/).

## Migration from retired execution

Oberth no longer implements technology-specific execution. Move VM creation,
device requests, guest harnesses, test gates and cleanup into repository
Workflows. Grant their operator ServiceAccount only the required namespaced
kinds/actions and extended resources. Existing KubeVirt grants do not become
capability grants.

Move the required test gate and cleanup into the Workflow, remove
`.oberth/tests.yaml`, `oberth.ci/kvm-templates` and guest-test annotations,
and arrange the capability grant and operator controls. The server does not
enforce the removed test contract; review and preserve the required test gate
in the repository Workflow.

`OBERTH_KVM` and `OBERTH_GUEST_TEST` are no longer injected. The five legacy MCP
tools and `oberth kubevirt` are removed. Historical runs, step usage and errors
remain readable without consulting the inert execution journals.

Drain old executions and clean up their external resources with the old owner
before upgrading. If an old execution is encountered at restart, Oberth fails
its run with a workflow capability migration message and stops its owned Argo
Workflow. It does not inspect or delete externally created resources. Operators
must remove any remaining resources. Stored `trustedTests`, `vm` and the
removed VM `argo` values refuse the upgrade until they are dropped; see
[removed chart values](../README.md#upgrading). Removed CLI serve flags must be
removed from custom launch commands.

No migration is added. Existing migration SQL remains byte-identical; retired
tables are inert and reserved for removal by a later migration. Take a database
backup before upgrade; rollback requires restoring that backup together with
the previous server. See [backup and restore](backup-restore.md).
