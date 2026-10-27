# Administrator-configured release tag dispatch

Default behavior is unchanged: admitted tags run `.oberth/release.yaml`;
branches, promotions and schedules run `.oberth/build.yaml`. Files and patterns
in repository YAML cannot alter dispatch or grant release credentials.

An administrator can configure `argo.releaseTagRoutes` in Helm values:

```yaml
argo:
  releaseTagRoutes:
    github/example/project:
      - pattern: demo-deploy-*
        file: .oberth/demo-deploy.yaml
```

Standalone servers use `--argo-release-tag-routes-config=/absolute/routes.json`
with the same map encoded as JSON. The chart mounts a public ConfigMap read-only
and checksums it for rollout. The server validates and freezes the configuration
at startup. Relative config paths, unknown/case-folded fields, duplicate JSON
properties, malformed globs and noncanonical repository names are refused.
The file is bounded to 1 MiB, 256 repositories, 32 routes per repository and
256 characters per pattern.

Patterns use Go path glob syntax (`*`, `?`, character classes), matched against
the durable bare tag name, guarded by `Run.RefKind == tag`, never a branch
with the same spelling. Receive handling strips `refs/tags/` before admission. `*` does not cross `/`.
Backslash escapes, spaces, tabs, CR/LF and `refs/` prefixes are disallowed. A tag matching
more than one entry is refused; list order never decides authority. An unmatched
tag uses the usual release file. Repository keys include upstream, organization
and repository; another upstream or sibling repository cannot inherit a route.

Files must be clean `.oberth/<DNS-label>.yaml` paths. Build, fragment, schedule and tests contract
files, nested paths, traversal and absolute paths are refused. The existing
bounded source-root reader reads the selected file at the tag's immutable tested
commit. A missing, unreadable or invalid selected file fails the run, with no
fallback to release.yaml. Routing does not change the fresh default-branch
reachability gate for tags or the release capability checks.

Each selected document declares its own scheduler weight and normal resource
contract. For example `.oberth/demo-deploy.yaml` can declare:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: M
spec:
  entrypoint: deploy
  activeDeadlineSeconds: 600
  templates:
  - name: deploy
    container:
      image: example.invalid/deployer@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: ["deploy"]
```

The static size, concurrency group, planned steps, credential detection, VM
resource inspection, admission audit and actual execution all select this same
file. Existing `oberth.ci/size-by-trigger` supports `tag=...` for release files.
No route-level size override bypasses the document's normal resource admission.
The audit records the selected `pipeline_file` alongside the executed commit.

Release preflight and the read-only release identity plan include the default
file and every configured route file. Changed routed documents get the existing
branch warning / promotion refusal capability checks. A missing configured
route file is reported as a problem. Grant declaration snapshots include these
files too. Local `oberth validate` retains its existing build/release inventory;
it does not load the server's administrator route configuration.

Routes select files, not identities. A leaf requesting WIF still needs the
administrator's exact repository/template/role mapping, and a WIF-only per-step
account also needs `step_routes.<template>` pinning the exact configured route
file and pattern. Another route (even one selecting the same file) and the
unmatched default release file cannot reuse that identity by copying its
template name. Legacy repository role maps retain their existing scope; use
distinct template names for distinct capabilities. See [release WIF](release-wif.md). No branch run can acquire an
identity by matching a release tag pattern.

A rollout changes the policy for future/unsubmitted work and explicit reruns.
An already-submitted Workflow retains its admitted immutable spec; automatic
recovery refuses a different intended spec rather than silently replacing it.
