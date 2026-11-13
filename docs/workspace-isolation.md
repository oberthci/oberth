# Isolating test steps from release authority

A workflow may need release credentials while its tests need only source and
private scratch space. Set the following fields on the test's Pod template:

```yaml
- name: release-test
  automountServiceAccountToken: false
  metadata:
    annotations:
      oberth.ci/workspace-mounts: none
      oberth.ci/workspace-env: none
  volumes:
  - name: private-scratch
    emptyDir:
      sizeLimit: 1Gi
  container:
    volumeMounts:
    - name: private-scratch
      mountPath: /work/private
    # Use your repository's approved digest-pinned image here.
    image: <approved-image>@sha256:<digest>
    command: ["/usr/local/go/bin/go"]
    args: ["test", "./..."]
    workingDir: /work/src
    env:
    - name: PATH
      value: /usr/local/go/bin:/usr/bin:/bin
    - name: GOCACHE
      value: /work/private/gocache
    - name: GOMODCACHE
      value: /work/private/gomod
```

`automountServiceAccountToken: false` prevents the pipeline token from being
mounted even in a credentialed workflow. Oberth retains the forced
ServiceAccount name, omits injected Vault environment, CA and server-binary
mounts, and does not inject the credential-chain token or secret tmpfs mount.
Recognized `oberth secretstore exec` commands that contradict
the opt-out are rejected. Argo's separate executor identity remains confined
to its init/wait containers. No new Kubernetes or OpenBao authority is granted.

A workflow-level or `templateDefaults` token opt-out applies to all named
templates. A template opt-out also applies to its inline children. A child
`true` cannot override an inherited `false`. Named template calls do not
inherit their caller's template fields; put the opt-out on the called leaf.

The only admitted template annotations are:

| Annotation | Value | Effect |
| --- | --- | --- |
| `oberth.ci/workspace-mounts` | `none` | Omit automatic work, persistent cache and collected-artifact mounts. Also omit their injected environment. |
| `oberth.ci/workspace-mounts` | `cache` | Like `none`, but keep the per-repository, tier-scoped persistent build cache at `/work/cache` and always set `GOMODCACHE`, `GOCACHE` and `OBERTH_CACHE_DIR` inside it. |
| `oberth.ci/workspace-mounts` | `readonly` | Make every work, persistent cache and collected-artifact mount read-only, including aliases and executor mirrors. |
| `oberth.ci/workspace-env` | `none` | Preserve authored tool/cache environment instead of injecting workspace defaults. Fixed run/release identity variables remain server-owned. |

`none` and `cache` preserve explicitly declared, non-reserved mounts, such as a private
test directory on the `work` claim. Server-reserved source, cache, artifact,
token and secret volume names cannot be used to introduce alternate mounts. It is an opt-out of automatic mounts, not
a claim that the Pod has no shared storage. Source remains read-only at
`/work/src`; bounded per-Pod private scratch remains writable. Declare private cache
locations when automatic caches are absent or read-only.

The run's source and artifacts paths share one Pod volume for their PVC.
Its claim-level `readOnly` stays unset so artifacts can be written; every
source `volumeMount` and executor source mirror is read-only. The artifacts
subpath has its own mount and follows the leaf's workspace controls. Using
one volume avoids duplicate PVC aliases that can stall kubelet mount setup
on local PVs.

### Keeping only the build cache

`oberth.ci/workspace-mounts: cache` is for build, test and lint steps that
want isolation from the shared `work` volume but should not start cold:

```yaml
- name: unit-test
  automountServiceAccountToken: false
  metadata:
    annotations:
      oberth.ci/workspace-mounts: cache
      oberth.ci/workspace-env: none
  volumes:
  - name: private-scratch
    emptyDir:
      sizeLimit: 1Gi
  container:
    volumeMounts:
    - name: private-scratch
      mountPath: /work/private
    image: <approved-image>@sha256:<digest>
    command: ["/usr/local/go/bin/go"]
    args: ["test", "./..."]
    workingDir: /work/src
    env:
    - name: GOCACHE        # used only when the tier has no cache root
      value: /work/private/gocache
    - name: GOMODCACHE
      value: /work/private/gomod
```

The leaf receives the read-only source at `/work/src`, the bounded per-Pod
scratch storage, and the writable cache at `/work/cache`, and nothing else
automatic: no `work` subpaths (tools, per-run fallback caches, Trivy cache,
release directory), no collected-artifact mount and no `OBERTH_ARTIFACTS`.
The workspace tool defaults (`PATH`, `OBERTH_TOOLS_DIR`, `GOPATH`,
`TRIVY_CACHE_DIR`, `GOLANGCI_LINT_CACHE`, ...) name paths the leaf does not
mount, so they are withheld exactly as with `none`.

`GOMODCACHE=/work/cache/gomod`, `GOCACHE=/work/cache/gobuild` and
`OBERTH_CACHE_DIR=/work/cache` follow the mount: they are set whenever the
cache is mounted, whatever `oberth.ci/workspace-env` says, and they override
authored values for those names. `workspace-env: none` together with `cache`
therefore means "my own tool environment, the server's cache". When the
run's tier has no cache root configured, there is no mount and none of the
three is set; authored private locations such as the configured private Go cache then apply,
and the step simply starts cold.

`cache` adds no mount and no authority that the default (no annotation) does
not already give. The cache is the existing one: one directory per repository
under the trigger's own root, so a branch build and a release build never
share it.

The CI-tier trust model is this: within one repository and tier, the cache is
written by every run. Any pusher's branch run can plant `GOCACHE` objects
(compiled packages, test binaries) that the next run on that tier, including
a `main` run, links in. This is already true of every default-mode leaf.
`cache` makes no promise of integrity against other pushers to the same
repository. For that reason `cache` is refused on a CI-tier leaf that runs a
credential chain (`oberth secretstore exec`): a
planted object would execute with the leaf's secrets in its environment. Use
`none` there. On the release tier `cache` is admitted on credentialed leaves
too, because the release root is written only by tag runs. Go's module and build caches tolerate
concurrent writers. Oberth's branch lint wrapper also keeps its versioned
golangci-lint v2.12.2 result cache here: that pinned tool uses a Go-derived
cache with local multi-process file locking. This requires the existing
node-local filesystem, not an NFS cache. With no configured cache root, lint
uses private scratch instead. Tool binaries remain on the per-run claim. A
Trivy `--cache-dir` inside `/work/cache` is shared across concurrent runs of
the same repository, so keep Trivy's database in private scratch.

`cache` is refused on leaves that never take a host-backed mount:
`oberth.ci/nonroot-templates` leaves (whose Go caches move to their
private scratch). Keep `none` for steps that must not see writable shared state, such
as signing, publishing, credential handling, and fetching or verifying
tools: a writable cache shared with every other build step of the
repository is exactly such state.

An explicit `readOnly: true` on any declared volume, including a dedicated
tools PVC, also survives server replacement at the same mount path. Oberth protects overlapping subpaths,
parent mounts, and aliases in sibling containers. A dynamic `subPathExpr`
is conservatively treated as overlapping. The server supplies a Pod patch
after Argo's mount mirroring to keep those wait-container mirrors read-only.
Repository-authored Pod patches, identities and security contexts remain
forbidden.

A sequential init container may write a template-local `emptyDir` that main or
sidecar containers mount read-only: init populates the tools before those readers
start. Oberth keeps the readers and their aliases read-only without propagating
their request backward to that init container. Explicit init read-only requests
still protect overlapping init aliases. This exception does not apply to PVCs,
workflow-level volumes, shared workspace/source or server-owned mounts, or
restartable init sidecars; their existing protections remain in force.
A sequential writer before a restartable init reader of the same protected
volume is also forced read-only. That conservative shape can fail the write;
it fails closed rather than introducing an order-sensitive exemption.

Controls apply to container, script and container-set leaves, including their
init containers and sidecars and leaves inside inline DAG/steps. Put workspace
mirroring in `mirrorVolumeMounts: true` when needed: Oberth resolves it once,
keeping an explicitly declared mount at a conflicting path and copying other
main mounts before credential injection. Argo-generated credential or staging
mounts are not copied into repository helper containers. Put workspace
controls directly on each Pod leaf; placing them on a DAG/steps parent is
rejected. Workspace isolation together with `templateDefaults` is rejected
because Argo merges those fields later. Declare the Pod fields on each leaf.
Mount declarations and enabled helper mirroring in `templateDefaults` are
always rejected: late merges could expose generated credentials. Declare
those mounts and helpers directly on each Pod leaf. Token-only defaults
remain supported; a tokenless leaf cannot inherit a credential chain from
defaults. Existing denials of resource/data/plugin/HTTP
templates and Argo artifact repositories remain in force.

The regression fixtures were captured by the actual Argo 4.0.8 controller
against fake Kubernetes clients. They cover the submitted Pod, including
the controller's mount and token mutations, and all eight captured shapes pass
Kubernetes 1.35.7 Pod-create validation. Oberth resolves duplicate helper mirrors
and supplies container-set volume definitions before submission. They do not establish behavior
of cluster admission webhooks or container execution. Regenerate that proof
when upgrading the controller; see `internal/argojob/testdata/isolation-argo4.0.8`.

## Server helper mounts

Authored mounts cannot use `/run/oberth`, its conventional image alias
`/var/run/oberth`, or anything beneath either path. This includes read-only mounts
and volume-device paths. Parent mounts such as `/run`, `/var/run`, `/var` and `/`
remain available, including a writable tmpfs at `/run` for a read-only image root.
The compiler appends the dedicated read-only assist helper mount after authored
mounts, and the pinned Argo Pod builder preserves that parent-before-child order.
This proves generated mount ordering, not the behavior of arbitrary runtime
implementations or filesystem symlinks in repository images.
Every authored mountPath and devicePath must be absolute, as Kubernetes requires.
Runtime-templated mount and device paths are refused because their resolved
paths cannot be checked at admission. Literal segment siblings such as
`/run/oberth-other` and `/run/oberth-gar` remain available.

The rule applies to every container, script, container-set common/member mount,
init container, sidecar and inline leaf. Existing held Pods are also checked for
helper shadows before being marked patch-capable. A shadowed hold remains
inspect-only for patching; release uses the image's `touch` fallback. These
checks protect declared mount paths, rather than attesting arbitrary filesystem
symlinks inside an authored image.
