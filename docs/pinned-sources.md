# Exact cross-repository sources

A workflow can request read-only source trees from other registered repositories.
The administrator must explicitly allow each consumer/producer pair. Empty
`sourceAccess` denies all pairs, including repositories in the same organization.
Names always use `upstream/org/repository`; aliases, URLs and credentials are
not accepted.

```yaml
# Operator chart values; these permissions are not workflow declarations.
sourceAccess:
  forge/acme/consumer:
    - forge/acme/producer
```

The equivalent server argument is
`--source-allowlist='{"forge/acme/consumer":["forge/acme/producer"]}'`.
Policy changes take effect when the server loads its configuration. Existing
running workflows retain their exact mounted sources; revocation prevents new
source preparation, including a user-requested retry that creates a new workflow.

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Workflow
sources:
  - repository: forge/acme/producer
    commit: "0123456789abcdef0123456789abcdef01234567"
    path: /work/sources/producer
spec:
  entrypoint: check
  templates:
    - name: check
      container:
        image: example.invalid/checker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: ["check", "/work/sources/producer"]
```

Replace the illustrative image and commit with real immutable identities. The
commit must be a full lowercase 40-hex **commit object**, reachable from the
producer's refreshed upstream default branch. Branch names, tag names and tag
object IDs are refused. This initial implementation deliberately requires
published default-branch ancestry; it does not expose arbitrary private refs.
A missing commit, stale upstream refresh, denied pair or failed export fails the
run before step execution. It never substitutes another revision.

`sources` is an Oberth extension removed before native Argo validation. Its
canonical `oberth.ci/sources` annotation is a declaration, not authority. Declare
sources in the consuming workflow. Every container, script,
container-set member and authored helper receives every declared source at a
unique `/work/sources/name` path. Per-step selection is not implemented.
Conflicting repository mounts are refused. Native retry and restart adoption
check the retained source bindings and read-only mount footprint.

Preparation uses the server's existing registered upstream access and bare Git
object cache. It exports exact blob objects, without `.git`, credentials or an
upstream URL in the Pod. Git archive attributes cannot omit or substitute files.
The existing server-to-PVC exec stream delivers those bytes; steps receive no
extra identity token, secret, RBAC grant or Git-fetch capability. The normal
per-run claim cleanup applies. Temporary server exports are removed after create
returns or fails. Git's existing object cache supplies commit reuse; there is no
separate unbounded archive cache.

Limits are four sources, 128 MiB and 4,096 regular files per source, a 1 MiB tree
inventory and a two-minute total preparation context. Paths, file modes and blob
lengths are checked. Symlinks, submodules and Git metadata paths are refused
rather than partially exported. The per-run source claim must have space for the
main checkout, up to 512 MiB of additional source content and normal helpers;
these bounds do not enlarge the configured claim or workload resources.

`run_get` (full detail, `summary:false`) exposes `source_bindings`, containing the producer, exact commit and
mount path. The same public identities appear in the submission audit and run
log. The immutable database record proves verified source selection before
allocation; it alone does not prove a Pod ran or that a hosted filesystem was
read-only. Unknown historical rows remain unknown. Migration
`20261105180000-pinned-source-bindings` adds the record without changing previous
migration bodies or receipts. Rollback requires a compatible pre-upgrade backup.

Hosted read-only filesystem acceptance remains unproven by local tests. On an authorized executor,
retain the Workflow/Pod UIDs and actual image IDs, verify a known producer file,
require a write attempt under the source mount to fail, and confirm no producer
credential or token was injected. Negative cases must fail before any step starts:
unlisted producer, wrong commit, unreachable commit and an oversized source.
