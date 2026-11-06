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
their environment with `env -i` and use private bootstrap scratch
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

`pins/bootstrap-tools.sha256` pins Linux amd64 executables. Go 1.26.10 builds
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

GAR actions use release-only Workload Identity Federation (WIF), selected by
`oberth.ci/release-wif-role` on the admitted leaf. The public
`scripts/oberth-release-wif-credential.py` helper exchanges the projected JWT at Google
STS, impersonates the administrator-configured source principal, then delegates
to the matching Oberth principal. Final accounts below belong to
`skipopsmain.iam.gserviceaccount.com`; source placeholders refer to full service
account emails held only in the operator installation configuration.

| Release template | Role / audience pool | First-hop principal | Final principal |
| --- | --- | --- | --- |
| `release-publish-images` | `image-writer` / `oberth-release-images` | `<release-publisher>` | `oberth-release-publisher` |
| `release-publish-chart` | `chart-writer` / `oberth-release-charts` | `<chart-publisher>` | `oberth-chart-publisher` |
| `release-verify` | `image-reader` / `oberth-release-reader` | `<release-reader>` | `oberth-release-reader` |

Each audience is
`//iam.googleapis.com/projects/138624361369/locations/global/workloadIdentityPools/<pool>/providers/<provider>`.
The project and the three pools above are fixed. Provider names are owned by
the deployment and must match `[a-z][a-z0-9-]*`; retain existing provider values.
Admission binds the exact repository, release ServiceAccount, template and role;
branch pipelines receive no WIF projection. The helper validates the fixed ADC
at `/run/oberth-gar/adc.json` and projected token at `/run/oberth-gar/token`,
requires `OBERTH_RELEASE_WIF=1`, and validates the ADC's first-hop URL against
an exact Google IAM endpoint/account pattern. The final target
is fixed by the validated pool. Both IAM requests use a 600-second lifetime.
Google enforces the existing matching TokenCreator and repository IAM bindings; the reader stays
read-only. HTTPS uses verified TLS 1.3, without redirects or proxies. A failed
exchange is fatal, with no fallback to another role or a static key.

Before the next release, the administrator must retain/set these existing keys in
**operator-owned** Helm values (`argo.releaseWIF.roles`) or the administrator JSON
file passed to `--argo-release-wif-config` (`roles`):

| Role key | `service_account` value |
| --- | --- |
| `image-writer` | `<release-publisher>`: existing full first-hop publisher email |
| `image-reader` | `<release-reader>`: existing full first-hop reader email |
| `chart-writer` | `<chart-publisher>`: existing full first-hop chart publisher email |

Use the current deployed source identities, preserving provider values, IAM,
repository/template grants and role separation. Never replace these source
accounts with the final Oberth accounts or commit their real values. The helper
reads the first-hop URL directly from the existing server-owned, read-only ADC
mount. This release works with today's server: no new environment variable,
installation key or preliminary server rollout is required.

Missing or malformed ADC source URLs refuse before network exchange with a
safe diagnostic naming `service_account_impersonation_url` and the install key.
The helper accepts any source account matching the strict endpoint pattern;
Google's existing WIF and IAM bindings enforce first-hop role permissions.
The final Oberth target remains fixed by the validated pool, and ADC cannot
override it. Confirm intended exchanges and provider-enforced cross-role
refusal in the next administrator-authorized live release; offline tests cannot
prove IAM bindings.

Only the final token reaches the private Docker credential-helper pipe.
Docker/Helm configuration contains the public helper mapping, not an access
token; registry login and token caching are not used. Image and chart
publishers still obtain `cosign-secret` through Secretstore into private tmpfs
for signing. The verifier uses the committed public signing key and does not
invoke Secretstore. The retired `gar-image-key`, `gar-reader-key`,
`gar-chart-key` and older `gar-sa-key` paths are not active prerequisites and
must not be recreated. Legacy code documents only an explicit reviewed source
rollback; it does not authorize restoring retired credentials or grants.

The credentialed artifact leaf publishes signed objects to the
Oberth-owned `oberth-releases` bucket. Its fixed phases run public upload, then
finalization after the credential-free runtime proof.
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

## Dashboard test runtime

The tokenless fetch-node and verify-node steps stage the pinned Node archive in
bootstrap-node-inputs. The release-test-rest leaf mounts that claim read-only and
verifies its private executable before running dashboard JavaScript tests.
The artifact publisher has no Node tooling or site deployment authority.
