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
  container:
    # Use your repository's approved digest-pinned image here.
    image: <approved-image>@sha256:<digest>
    command: ["/usr/local/go/bin/go"]
    args: ["test", "./..."]
    workingDir: /work/src
    env:
    - name: PATH
      value: /usr/local/go/bin:/usr/bin:/bin
    - name: GOCACHE
      value: /tmp/gocache
    - name: GOMODCACHE
      value: /tmp/gomod
```

`automountServiceAccountToken: false` prevents the pipeline token from being
mounted even in a credentialed workflow. Oberth retains the forced
ServiceAccount name, omits injected Vault environment, CA and server-binary
mounts, and does not inject the credential-chain token or secret tmpfs mount.
Recognized `envconsul` or `oberth secretstore exec` commands that contradict
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
| `oberth.ci/workspace-mounts` | `readonly` | Make every work, persistent cache and collected-artifact mount read-only, including aliases and executor mirrors. |
| `oberth.ci/workspace-env` | `none` | Preserve authored tool/cache environment instead of injecting workspace defaults. Fixed run/release identity variables remain server-owned. |

`none` preserves explicitly declared, non-reserved mounts, such as a private
test directory on the `work` claim. Server-reserved source, cache, artifact,
token and secret volume names cannot be used to introduce alternate mounts. It is an opt-out of automatic mounts, not
a claim that the Pod has no shared storage. Source remains read-only at
`/work/src`; bounded per-Pod `/tmp` remains writable. Declare private cache
locations when automatic caches are absent or read-only.

An explicit `readOnly: true` on any declared volume, including a dedicated
tools PVC, also survives server replacement at the same mount path. Oberth protects overlapping subpaths,
parent mounts, and aliases in sibling containers. A dynamic `subPathExpr`
is conservatively treated as overlapping. The server supplies a Pod patch
after Argo's mount mirroring to keep those wait-container mirrors read-only.
Repository-authored Pod patches, identities and security contexts remain
forbidden.

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
