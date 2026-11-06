# Operator-owned workflow capabilities

Oberth runs repository Workflows. Pipelines own any objects they create, their
cleanup, and their test gates. An administrator can grant one repository an
operator-created identity for namespaced resource templates or qualified
extended resources. Ordinary CI requires no such grant.

The operator creates the ServiceAccount in the configured execution namespace,
its least-privilege resource RBAC, Argo task-result permissions, and namespace
ResourceQuota. The server needs permission to request tokens for that exact
account. For new capability workflows, operators provide their infrastructure and
resource permissions. Kubernetes API discovery must identify every
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
Container, script and helper steps keep the tokenless pipeline identity. Only
the admitted resource template's argoexec containers receive the operator token;
any other container that mounts it causes token delivery to be refused.
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

## Migration from retired execution

Oberth no longer implements technology-specific execution. Move VM creation,
device requests, guest harnesses, test gates and cleanup into repository
Workflows. Grant their operator ServiceAccount only the required namespaced
kinds/actions and extended resources. Existing KubeVirt grants do not become
capability grants. Remove `oberth.ci/kvm-templates`, guest-test annotations and
`.oberth/tests.yaml` after moving the required gate into the Workflow. The server
checks both candidate and upstream default policy, so deleting a gate in a
candidate cannot silently bypass the default branch requirement.

`OBERTH_KVM` and `OBERTH_GUEST_TEST` are no longer injected. The five legacy MCP
tools and `oberth kubevirt` are removed. Historical runs, step usage and errors
remain readable without consulting the inert execution journals.

Drain old executions and clean up their external resources with the old owner
before upgrading. If an old execution is encountered at restart, Oberth fails
its run with a workflow capability migration message and stops its owned Argo
Workflow. It does not inspect or delete externally created resources. Operators
must remove any remaining resources. Old `trustedTests.*`, `vm.*` and retired
`argo` values are accepted, warned about and ignored; they cannot restore runtime
or RBAC. Removed CLI serve flags must be removed from custom launch commands.

No migration is added. Existing migration SQL remains byte-identical; retired
tables are inert and reserved for removal by a later migration. Take a database
backup before upgrade; rollback requires restoring that backup together with
the previous server. See [backup and restore](backup-restore.md).
