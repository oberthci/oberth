# From-67 release pins and hand-stamped charts

The operator's v0.17.62 CLI is unsuitable. Do not execute it for backup/restore.
Use absolute paths to separately downloaded and verified executables. The
from-67 runbook checks executable SHA256 before its first invocation, including
in a fresh recovery shell. `version --schema` reports binary support, not the
actual DB schema; inspect the encrypted history archive for the latter.

The rehearsal verified the official v0.17.67 artifacts at
https://releases.oberth.ci/oberth/v0.17.67/release.json and its detached Sigstore
bundle, plus SHA256SUMS and its bundle. ECDSA/SHA256 message signatures were
checked with the trusted source-pinned `.oberth/pins/release-cosign.pub` (key
file SHA256 `036dd71c9d3c07a19bff06d4392874014573f695620ac8554d9df175094c4a31`).
Both downloaded arm64 CLIs matched both signed payloads. This is pinned-key
message verification, without transparency-log verification. Preserve the
verification script, payloads, bundles and result with the release handoff.

| Artifact | Verified pin |
| --- | --- |
| Official v0.17.67 annotated Git tag object | `423c90796b1581d71a353177e696125bb71c60f1` |
| Peeled v0.17.67 source | `5e567d5c1f60c001c43bc4d07a9255bac44f4b25` |
| Published v0.17.67 image | `europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:86070b32caf9c301bddb69515dcaceceb7c57483b743686ba002ccfa9ea0cc7d` |
| Published Darwin arm64 CLI SHA256 | `776adaa77f4a99eaae02770af7a3b61604d36d18748a2894e2025d84ad936820` |
| Published Linux arm64 CLI SHA256 | `1e46661838c0979e8a5102fc9553f4bcd0fd00de0371c5864c5650f7802af3f2` |
| Published Linux amd64 CLI SHA256 (signed metadata; not downloaded in this rehearsal) | `11f36ae834a6a34f3ef66c5990fff7c6b4aff4906d2f85981e5285ae5ba8a530` |
| Candidate source (unchanged) | `863a88eb80244f0d7cc8ee96852e3e1e64381f89` |
| Rehearsal v0.17.73 image (local build, not a published release) | `oberth-v073-final@sha256:86a069eb74708da29d3bfcc917173f874cb4731ff6adf7ce613878f53664513c` |
| Rehearsal v0.17.73 Darwin arm64 CLI SHA256 | `a9928dfebbca60a462c5dbaad8eb4c83425e9509e4fe0ca0f4d01288a49b1a82` |
| Rehearsal stamped v0.17.73 chart SHA256 | `c74eb830d51439d6932206acdbc5abee4c8aa9eab93545d6dd437a4b29fb1a0f` |

The old side uses the actual published image, including its original OCI index
digest, not a source rebuild with an old version label. The new side reuses the
exact 863a88eb image/CLI/chart from the earlier final-head qualification. There
is no published v0.17.73 tag/image attestation in this rehearsal. Production must
substitute verified published pins for that same approved source, not use the
local rehearsal image reference. Select the operator CLI checksum for the
operator's architecture; a Darwin checksum does not verify a tuxbox Linux CLI.

## Prepare chart directories manually

Tuxbox uses Helm with Git-tag chart templates and its reviewed live values,
not `oberth upgrade`. Start with a clean trusted checkout. Verify the old tag
peels to the full SHA above; verify the eventual new tag against the approved
candidate source. Extract each tag's `charts/oberth` into separate private
working directories with `git archive TAG charts/oberth`. Before packaging,
edit only these release pins in each extracted chart:

| Extracted chart | Chart.yaml version | Chart.yaml appVersion | values.yaml image.ref |
| --- | --- | --- | --- |
| v0.17.67 | `0.17.67` | `v0.17.67` | exact published old digest above |
| v0.17.73 | `0.17.73` | `v0.17.73` | verified published new digest for 863a88eb |

The actual old tag's raw Chart.yaml says **0.17.5/v0.17.5**; candidate source
says **0.17.70/v0.17.70**. These are stale in-tree metadata, not deployment
pins. Record the extraction source, exact diff, packaged chart SHA256 and
rendered image/probes/roles. Hand-stamp before `helm package`; archive the normal
old chart unchanged thereafter. The emergency zero-replica maintenance copy
later makes exactly one additional Deployment-template edit, checked separately.

Export live values with `helm get values oberth -n NAMESPACE --all` using the
explicit kubeconfig/context. Keep an old-values copy valid against the old
chart, and build a separate new-values file preserving install identities,
storage, trusted profiles, namespaces and controller settings. Review new
capacity/PSI defaults and probe/RBAC changes. Render/dry-run using those full
files; `--reset-values -f FILE` avoids inheriting new-only keys during recovery.
Both installed and rendered image digests must match the intended version.

The real v0.17.67 CLI prints `commit=5e567d5c1f60` and support schema33. It
must reject the post-upgrade schema35 archive. Ordinary Helm rollback and SQL
index/ledger edits are not recovery procedures. Follow the named old-chart,
old-CLI, zero-replica history-restore sequence in the from-67 runbook.
