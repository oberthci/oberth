# Operator-owned workflow capabilities

Oberth runs repository Workflows. Pipelines own any objects they create, their
cleanup, and their test gates. An administrator can grant one repository an
operator-created identity for namespaced resource templates or qualified
extended resources. Ordinary CI requires no such grant.

The operator creates the ServiceAccount in the configured execution namespace,
its least-privilege resource RBAC, Argo task-result permissions, and namespace
ResourceQuota. The server needs permission to request tokens for that exact
account. For new capability workflows, operators provide their infrastructure and
resource permissions. The deprecated VM path retains its existing chart RBAC. Kubernetes API discovery must identify every
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

The old VM path is deprecated and its deletion is deferred. `OBERTH_KVM`,
`OBERTH_GUEST_TEST`, guest-test injection, `.oberth/tests.yaml` suite and release
publication gates, VM launcher RBAC, cleanup and restart recovery remain active.
Existing KubeVirt grants retain their original bounded authority; they do not
authorize generic resources or select an operator identity. New capability
grants are separate and start empty. Legacy chart values and CLI flags continue
to work. The inactive pilot remains retired under Schema 45.

Migrations 45/46/47 retire the inactive pilot, add the capability grant table,
and establish the staged upgrade boundary without retiring the live VM journal.
Back up the database before upgrading. The v0.17.84 CLI requires legacy
preflight Workflows and Oberth-owned VMs to be absent before upgrade; see the
[retirement upgrade procedure](retirement-upgrade.md). Work in flight remains journaled and
reaches the same owner recovery used after a restart. Startup cancellation
cleanup retries external deletion failures with two-minute attempt budgets and
backoff capped at five seconds. The process stays alive and unready, with
`recovery_pending=true` in its logs, until cleanup succeeds. Permanent database
invariant failures remain fatal. See the [staged upgrade procedure](releases/unreleased-execution-retirement.md).

Stage 2 requires operators to provision resource/device capacity, accounts and
RBAC, grant the replacement capability, move guest and suite gates into their
repository Workflows, and prove cleanup before removing the deprecated path.
That deletion is deliberately absent from this release. Rollback requires the
documented database restore procedure, not only an older image.

Trusted preflight observation uses semantic Kubernetes resource equality. An
owned spec mismatch remains a run-local failure: Oberth binds its observed UID
for cleanup only, retains admission and retries exact-run cleanup until absence
is confirmed. Foreign run/source identities never authorize deletion. This
preserves the original trusted observer path while Stage 2 deletion is deferred.
