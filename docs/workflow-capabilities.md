# Operator-owned workflow capabilities

Oberth runs repository Workflows. Pipelines own any objects they create, their
cleanup, and their test gates. An administrator can grant one repository an
operator-created identity for namespaced resource templates or qualified
extended resources. Ordinary CI requires no such grant.

The operator creates the ServiceAccount in the configured execution namespace,
its least-privilege resource RBAC, Argo task-result permissions, and namespace
ResourceQuota. The server needs permission to request tokens for that exact
account. The chart does not grant technology-specific permissions or create
the operator's infrastructure. Kubernetes API discovery must identify every
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
are opaque to Oberth; operator RBAC and quota constrain their effects. The
allowlist supports `create`, `get`, `delete`, and `apply`.

The server assigns the operator identity and its resource executor. It refuses
shared pipeline, executor, secret-store and per-repository secret identities.
Operator capability workflows cannot also consume secret-store or WIF grants.
Grant scope and identity are checked again before create, adoption and executor
token delivery. Revocation prevents new authorization; it does not delete
objects already created by a pipeline.

Extended-resource requests and limits must be equal positive integers. Configure
the corresponding whole-server capacity through
`admission.declaredResources.extendedResources`, or
`--admission-extended-resources='{"example.invalid/accelerator":2}'`.
The bounded graph accounts overlapping steps, fan-out, helpers and parallelism.
Its independent resource peaks must fit both the repository grant and the
server's capacity. Unbounded graphs, missing budgets or declarations cannot
fall back to size admission for extended resources. Kubernetes still enforces
actual device availability and namespace quota.

Before upgrading from VM support, drain old runs, confirm external cleanup,
and move test gates and object lifecycle into repository Workflows. Validate
those destination pipelines before deploying the retirement. Schema 47 retains
old execution journals, capacity rows and grants as read-only history without
inventing cleanup or transferring authority. Export history before a later
operator-managed archival migration. Old `vm`, `trustedTests`, and Argo VM
Helm values are accepted but ignored with a warning; removed direct CLI flags
are no longer accepted. `.oberth/tests.yaml` has no special meaning. Rollback
requires the documented database restore procedure, not only an older image.
