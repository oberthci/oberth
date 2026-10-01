# Release bootstrap contract

The release workflow requires v0.16.6 or later admission. Its template-level
token and workspace controls isolate each publisher from tests and downloaded
artifact execution.

The workflow deliberately has no claim named `work`. That name enables old
server mounts in every leaf. Five existing test commands keep their original
arguments and dependency gates. Tests receive independent copies of tools;
the native artifact build and public runtime check receive their own copies
too. No test or public runtime Pod receives the publisher tools or candidate
output claim, including through Argo's init and wait containers. The builder
receives only its intended output claim and its copied tools.

The old server still injects shared cache/artifact mounts. Commands replace
their environment with `env -i` and use private `/tmp/bootstrap-private`
caches, ignoring those mounts. Credentialed commands use system PATH and
absolute publisher executable paths. An init container verifies publisher
source blobs against the admitted commit/tag and checks all five executable
hashes before `secretstore exec` runs. No repository test can change that
tools claim. Argo wait mounts are not assumed read-only.

The selected release ServiceAccount must have
`automountServiceAccountToken: false`. The old controller leaves Pod automount
unset, so this is an operational prerequisite, not a YAML guarantee. Only
recognized secretstore main containers get the explicit release token.
Controller construction and Kubernetes ServiceAccount admission must both be
checked; a false-SA positive control and true-SA adverse control distinguish
this dependency from the newer server's tokenless-template behavior.

Public runtime verification fetches signed versioned artifacts into private
scratch, verifies the committed public key and strict checksum manifest before
and after executing the native binary, then records the exact source and
artifact identities. It receives a separate receipt claim, no publisher tools,
no candidate output, and no release token or secrets. Finalization reads that
receipt and runs under a fixed repository-scoped mutex. It accepts only its
own tag's candidate bytes; a newer marker never causes it to execute or
promote a different release's bytes.

## Tool pins

`pins/bootstrap-tools.sha256` pins Linux amd64 executables. Go 1.26.6 builds
the two repository helpers and Helm v4.2.3 with `CGO_ENABLED=0`, `GOOS=linux`,
`GOARCH=amd64`, `GOTOOLCHAIN=local`, `GOWORK=off`, `GOENV=off`, `-trimpath`,
`-buildvcs=false`, and `-ldflags=-buildid=`. The helper builds use
`-mod=readonly`. Two independent builds with clean source, module extraction,
and build caches must agree before a pin is updated. Module zip proxies may
cache downloads; extracted source and build outputs must not be shared with
repository tests.

Helper inputs are the current committed `go.mod`, `go.sum`,
`cmd/oberth-release-support`, `cmd/oberth-release-image`,
`internal/releaseauth`, and `internal/releaseimage`, together with their module
dependencies. Every edit to these inputs requires new reproducible pins.
Helm is built from its exact versioned module. Cosign v3.1.1 uses the
existing pinned release executable; Trivy comes from the workflow's pinned
official v0.73.0 image. Every credentialed leaf checks all five pins.

## Publication prerequisites

GAR actions use separate OpenBao KV paths, all with field `GAR_SA_KEY`:

| Action | Path under `oberth/data/release/` | Fixed delegated service account in `skipopsmain.iam.gserviceaccount.com` |
| --- | --- | --- |
| `publish-images` | `gar-image-key` | `oberth-release-publisher` |
| `publish-chart` | `gar-chart-key` | `oberth-chart-publisher` |
| `verify` | `gar-reader-key` | `oberth-release-reader` |

The source keys remain in their existing, separate OpenBao paths. The pinned
release helper validates bounded service-account JSON and exchanges it at fixed
Google OAuth and IAM Credentials endpoints for a one-hour access token of the
listed principal. Terraform grants each source TokenCreator on only its matching
target. A source from another role is rejected by IAM; neither ambient ADC nor a
key-provided token URL can redirect authentication. Tokens and Docker/Helm
configuration exist only in the leaf's private memory filesystem and are cleared
on exit. No new private key or secret-store path is needed.

Each target has access only to its own GAR repository; the reader retains
read-only access. Preflight must prove the intended exchanges and cross-role
refusals with the exact release ServiceAccount and paths. The shared
`gar-sa-key` is never a fallback.

The existing website-authorized leaf also publishes signed objects to the
Oberth-owned `oberth-releases` bucket. Its fixed phases run public upload, then
finalization after the credential-free runtime proof, then website deployment.
The signer leaves receive no Cloudflare token. Artifact snapshots are bounded,
read-only at the source, signature-checked before upload, and never executed in
the credentialed leaf. Finalization retains its shared mutex and conditional
index/latest updates. Public URLs are `https://charts.oberth.ci` and
`https://releases.oberth.ci`.

Both GAR repositories (`oberth` and `oberth-helm`) must actually enforce
immutable tags before a bootstrap tag is submitted. Source digest checks and
readback reject mismatches but cannot close a check/write race on mutable
registries. Matching image/chart retries reuse existing content and verified
digest signatures. Versioned R2 objects use conditional create and exact
readback; mutable R2 aliases retain their conditional update protocol.

Final local CI, independent review, native CI/promotion, annotated release
tags, fetched-artifact verification, and deployment authorization remain
separate gates. Local controller fixtures are construction evidence, not a
claim of live Pod execution or an applied registry policy.

## Website publication (#649)

The oberth.ci Worker (`oberth-ci`, Workers Static Assets, custom domains
oberth.ci and www.oberth.ci) is published by the release burn, not by hand.

- `release-setup` gains three credential-free steps. `fetch-node` downloads
  the official Node tarball named in `pins/node.url` into the dedicated
  `bootstrap-website-inputs` claim, `verify-node` checks it against
  `pins/node.sha256`, and `stage-website-packages` re-verifies a private copy,
  extracts it, proves the executable against `pins/node-bin.sha256`, and runs
  `npm ci --ignore-scripts` from the committed `website/package-lock.json`
  with only the npm cache written back to the claim. No package code runs;
  the claim holds only hash-checked or content-addressed inputs. These Pods
  are tokenless (`automountServiceAccountToken: false`) and receive no shared
  workspace mounts or environment.
- `release-website` depends on `release-verify`. Its first init container,
  `verify-website-inputs.py`, binds the publisher source (release.sh, the
  verifier, the Node pins, the npm manifest and lockfile, `wrangler.jsonc`
  and the exact `website/public` tree) to the admitted commit and tag, then
  copies the tarball (hashed while copied) and the npm cache into Pod-private
  `/tmp/oberth-website`. The second, `install-website-tools`, extracts Node,
  re-proves the executable pin, installs wrangler with
  `npm ci --offline --ignore-scripts` (every tarball re-verified against the
  lockfile's sha512) and requires the lockfile's exact wrangler version.
  Only then does the main container run `oberth secretstore exec` with its
  single path, `oberth/data/release/cloudflare-oberth-workers-token` (field
  `CLOUDFLARE_API_TOKEN`); it never receives a GAR key, the cosign secret or
  the publisher tools claim. `release.sh publish-website` re-proves the Node
  pin, refuses any `wrangler.jsonc` other than the reviewed Worker, and hands
  the token to wrangler through its environment only, with wrangler's home,
  config and logs on the memory-backed secret mount.
- `release-website-readback` is tokenless. It re-asserts the #647 contract on
  the admitted source (identical semantic audit preflight in both
  `setup-secretstore.sh` copies, no pre-#647 grep) and polls, without cache
  busting, until `https://oberth.ci/`, `https://www.oberth.ci/`, both
  `/setup-secretstore.sh` URLs and `/install.sh` serve the admitted bytes
  with their `_headers` content types (window 900 s, above the scripts'
  300 s max-age). A stale edge copy is not publication.

The deploy token is Terraform-owned (`oberthci/terraform`
`website-deploy-token.tf`: account Workers Scripts Write, read-only Zone Read
and Workers Routes Read on oberth.ci, no cache purge or DNS write). Its value
reaches OpenBao only through the administrator's custody step and the
release Pod only through `secretstore exec`.
