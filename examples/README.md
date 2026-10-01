# Oberth pipeline examples

These examples show how Oberth's Argo Workflow pipelines enforce
cross-compile identity, credential isolation, and tool verification
structurally. Each `.oberth/` directory contains standard Argo Workflow YAML
that Oberth submits with server-enforced metadata, ServiceAccount identity,
and source injection.

| Example | For | Key demonstration |
|---|---|---|
| [`go-project/`](go-project/.oberth/build.yaml) | Go repositories | Cross-compile identity via explicit GOOS/GOARCH per template, release credential isolation via trigger-gated ServiceAccount, envconsul credential delivery |
| [`generic/`](generic/.oberth/build.yaml) | Any language | SHA-256 verified tool install, per-template timeouts, no-shell exec model |

## What Oberth's enforcement catches

**Cross-compile identity fraud.** A YAML job named `build-arm64` that forgets
`GOARCH=arm64` ships a native amd64 binary. Nobody notices until a customer
on Apple Silicon reports `exec format error`. Oberth's admission validator
rejects Workflow templates with identical command, arguments, and environment
wearing different names.

**Credential scope.** Oberth forces the ServiceAccount identity server-side
by declared secret paths. Pipelines without approved paths bind to the
pipeline ServiceAccount with no credentials and
`automountServiceAccountToken: false`. Pipelines with approved paths bind to
the credentialed ServiceAccount that can authenticate to your secret store
via Kubernetes auth. Branch pipelines may only declare upstream-scoped
paths; system-namespace paths require a release trigger. The YAML cannot
override the identity.

**Opaque failure modes.** Each Argo Workflow template runs in its own
container with its own exit code and named log slice. A failure in the `lint`
template identifies itself by name -- not by "exit code 1" from an opaque
shell script.

## Pipeline files

Each example's `.oberth/` directory contains:

- `build.yaml` -- branch-trigger CI pipeline (Argo Workflow YAML)
- `release.yaml` -- tag-trigger release pipeline (where applicable)
- `pins/` -- SHA-256 checksum files for pinned tool downloads

Copy any `.oberth/` directory into your repository root, push through Oberth,
and the pipeline runs with admission validation before a Workflow starts.

## Ground rules

- **No shell.** Each template is a direct container command -- no bash, no
  word-splitting, no `set -e` scope holes.
- **Pins travel with the pipeline.** Tool versions and checksums are files in
  the same commit; bumping a tool is a reviewed diff.
- **Pipelines without declared paths get no credentials.** The pipeline
  ServiceAccount has no Vault/OpenBao access and no automounted token.
  Branch pipelines may only declare upstream-scoped paths.
- **Tags need a release Workflow.** A tag push selects `.oberth/release.yaml`;
  if it does not exist, the push is rejected.
