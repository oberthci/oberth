# YAML concurrency groups

Pipelines sharing infrastructure can share a concurrency limit across
repositories. Declare both settings on the Workflow:

```yaml
metadata:
  annotations:
    oberth.ci/concurrency-group: shared-state
    oberth.ci/concurrency-limit: "2"
```

The limit defaults to **1** when omitted. It is a positive decimal integer
(1–2147483647), and requires a group. Names use 1–63 lowercase letters, digits
or hyphens. Oberth reads the annotations from the run's immutable pipeline
YAML. There are no chart settings, administrator group definitions or built-in
group names. A resource or VM template does not imply membership.

The name is shared across repositories. Branch, tag, release and promotion
runs all participate. Each run uses the document it executes: build.yaml for
builds and release.yaml for releases. Every pipeline naming the same group
must declare the same limit, including the default. A conflicting declaration
fails admission with the group and both limits in the reason; no Workflow is
created. Rejected declarations do not replace accepted definitions.

Up to N members own a group slot at once. Waiting members retain priority/FIFO
order and hold no workflow slot or resource reservation. Group membership
does not bypass the global workflow ceiling, administrative hold, disk floor,
hard pressure valve or VM limits. Kubernetes places pods from their requests.
Running and retained grants keep their group slots until execution is known
to be terminal or deletion has been accepted. Cancellation before a grant
withdraws the waiter; VM parking releases the group slot.

The existing queued reason line names the group, limit and current holders.
No additional run status or badge is introduced. Read the shared state with:

```sh
oberth admission groups
oberth admission groups --json
oberth mcp call concurrency_groups '{}'
oberth mcp call system_status '{}'
```

The dedicated tool/CLI requires an unscoped uplink, without requiring an admin
role, and cannot mutate definitions. System status includes `admission.groups`.
Groups include `name`, `limit`, `holders` and `waiting` run IDs; idle definitions
have empty member lists. Scoped system status and run observations omit members
outside the caller's repository scope.

## Persistence and YAML updates

Oberth persists the group and limit on each run before granting it. The latest
accepted build and release declaration of each repository also survives run
completion and restart, so idle groups retain their definitions. The group list
reflects YAML that the scheduler has read, not an unobserved repository edit.
An accepted run whose YAML omits the group removes that pipeline's remembered
definition. Existing unfinished runs retain their immutable contracts.

To change a shared limit, all remembered declarations must agree, and old
unfinished members must drain first. When changing several pipelines, remove
the annotations and run those pipelines to clear their old definitions before
introducing the new matching limit. A rejected conflict leaves the earlier
accepted definition intact. Removing a repository removes its declarations.

Existing annotations keep their default limit of one and their trigger
membership. Equal names in different repositories now intentionally share the
limit. When upgrading with legacy runs already executing independently, Oberth
adopts every existing grant and admits no additional member until their combined
count is below the shared limit. It does not interrupt those runs.
