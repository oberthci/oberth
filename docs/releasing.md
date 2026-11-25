# Release a tested commit

## Release your project

Put your publisher in `.oberth/release.yaml`; provision [grants](pipelines.md#secrets-and-identity).
As administrator, promote a full commit ID from any directory with `--repo`.
Promotion can create a different default commit without moving local `HEAD`; save local work first.
Then fetch and check out the full resulting commit ID printed by promotion:
```sh
oberth promote <tested-commit-sha> --repo <repository>
git fetch -- oberth refs/heads/<default-branch>
git switch --detach <resulting-commit-sha>
# Wait for passing, unassisted CI; if default moved, inspect its head and evidence first.
oberth release v0.1.0
```

Release checks eligibility and grants before creating/pushing a tag; refusals create none. Follow
printed grant remedies and retry. Existing tags require the same commit. Confirm the run and
[publication receipt](operations.md#retention-and-publication).
`check --local` selects no release run or release-tier grants; `release` tags without a dry run.

## Agent promotion

Commit `uplinks_may_promote: true` in `.oberth/promotion.yaml` to permit uplink promotion; passing
unassisted evidence is still required, and direct default-branch pushes remain refused.

## Maintainer release preparation

Stable `vMAJOR.MINOR.PATCH` tags release after setup. The server supplies:

- `version`: the admitted tag, above successful or in-flight releases recorded here, never reused.
- `release-tested-commit`: the tag's peeled commit, for lightweight and annotated tags.
- `release-default-commit`: the exact default head used by the server's Git ancestry check.
- `release-build-status`: `passed` from the latest admitted `.oberth/build.yaml` run on that commit.

Missing, running, failed or assisted builds refuse before execution: run the build, then push again.
Outside releases are unknown; reruns keep facts but must clear the recorded version floor. Changed
identity or assistance refuses. Release arguments, defaults and global outputs cannot replace facts;
builds keep their own values. Offline validation proves no live facts. By hand is the fallback. See
[tag routing](pipelines.md#build-tags-and-release-tags) for file selection and ambiguity refusal.
Release files are assets of the tag's GitHub release. Set `release-config.origin` to
`https://github.com/oberthci/oberth/releases/download`, the
[canonical address](../internal/releaseverify/origin.go); scripts refuse other addresses.
Signatures are checked only against the committed pinned key; no public log is used.

## Release Oberth through Oberth

Push green CI, promote, wait for the promotion to deliver, then release strictly above `latest`.
The release pipeline prepares the base and signer, builds, signs, pushes both images to the
registry, publishes the files as the tag's GitHub release, verifies them in a clean Pod and advances
`latest`. The build checks its commit without rerunning tests. Only signing gets the long-lived key,
only the image push a federated login, only publish/finalize the GitHub token. Branch CI reaches
neither those credentials nor the release cache. Oberth puts the tag on GitHub when it admits the
release, before the run; that uses the version: rerun a failed run or release the next one.
Only an administrator may rerun a release run; agents can release the next version.

Edit [resources](../.oberth/pipeline-resources.yaml): storage and HTTPS origin; apply in the
pipeline namespace. Set the two `release-publish` mutexes in release.yaml to your repository
(`upstream/org/repo/release-publish`). Caches need uid/gid 65534 access. Then grant:

```sh
oberth access allow --kind pvc --tier ci '<repo>' 'build-cache'
oberth access allow --kind pvc --tier release '<repo>' 'release-cache'
oberth access allow --kind identity --audience 'oberth-release-registry' \
  '<repo>' '.oberth/release.yaml' 'release-images-pushed' 'identity'
oberth access allow --kind configmap '<repo>' '.oberth/release.yaml' \
  'release-images-pushed' 'release-config'
oberth access allow --kind configmap '<repo>' '.oberth/release.yaml' \
  'release-published-immutable' 'release-config'
oberth access allow --kind configmap '<repo>' '.oberth/release.yaml' \
  'release-verifies-from-clean-pod' 'release-config'
oberth access allow --kind configmap '<repo>' '.oberth/release.yaml' \
  'latest-advanced-monotonic' 'release-config'
oberth secret put --repo '<repo>' --file .oberth/build.yaml --step lint-and-guards-pass \
  oberth/policy/names < <private-names-file>
oberth secret put --repo '<repo>' --file .oberth/build.yaml --step lint-and-guards-pass \
  oberth/policy/identity < <identity-file>
```

Both stay outside Git; pushers can read them. The identity is the commits' one `Name <email>` line.
Registry pushes use workload identity federation:
[setup](https://cloud.google.com/iam/docs/workload-identity-federation-with-kubernetes).
An administrator configures the provider to trust the cluster issuer and audience
`oberth-release-registry`, restricted to the exact granted release step account's subject.
Allow that subject to impersonate a registry writer restricted to this image repository.
Set `release-config.registry-provider` to
`projects/NUMBER/locations/global/workloadIdentityPools/POOL/providers/PROVIDER`
and `registry-account` to the writer's email. A missing value refuses at the config stage.
Only the push step mounts a read-only 3600-second identity. `oberth-release registry-login`
exchanges it with STS, impersonates the writer for one hour and writes a private login in memory.
Exchange failure stops the push; no stored registry login is used. The release needs two secrets:

- `oberth/data/release/signing` (step `release-files-signed`): the pinned private `release.key`.
- `oberth/data/release/github-token` (steps `release-published-immutable` and
  `latest-advanced-monotonic`): a token with read/write contents on this repository only.

```sh
oberth secret put --repo '<repo>' --file .oberth/release.yaml --step release-files-signed \
  oberth/data/release/signing < <private-key-file>
oberth secret put --repo '<repo>' --file .oberth/release.yaml --step release-published-immutable \
  oberth/data/release/github-token < <github-token-file>
oberth secret put --repo '<repo>' --file .oberth/release.yaml --step latest-advanced-monotonic \
  oberth/data/release/github-token < <github-token-file>
```

Missing grants print remedies; creating objects grants none. `secretstore exec` delivers granted
files below `OBERTH_SECRETS_DIR` without exporting values. Check local YAML against live grants with
[check](pipelines.md#start-with-a-complete-file). The VM install walk,
[walk-vm.yaml](../.oberth/walk-vm.yaml), uses [file selection](pipelines.md). Its objects are in
[walk-vm-resources.yaml](../.oberth/walk-vm-resources.yaml), with its VM capability and SSH exception.

## First release by hand

Use Linux amd64, Go **go1.26.10**, Python 3, curl, Git, make, grep and `sha256sum` in a clean tree.
Keep work private. Allow 12 GiB and eight minutes (more cold), plus 0.7 GiB in `$TMPDIR`.
Have registry write access and a repository release-write token. Choose stable `vMAJOR.MINOR.PATCH`
above `latest` (404 means no floor):
```sh
export RELEASE_ORIGIN=https://github.com/oberthci/oberth/releases/download
curl --silent https://api.github.com/repos/oberthci/oberth/releases/latest | grep '"tag_name"'
export VERSION="<version-above-latest>"
export PATH="<go1.26.10-directory>/bin:$PATH" GOTOOLCHAIN=local
go version
export RELEASE_WORK="$HOME/work/oberth/release-$VERSION"
export RELEASE_PRIVATE="$RELEASE_WORK/private" RELEASE_SHARED="$RELEASE_WORK/shared"
export RELEASE_DIR="$RELEASE_WORK/dist" RELEASE_VERIFY_DIR="$RELEASE_WORK/verify"
export RELEASE_TOOL="$RELEASE_WORK/oberth-release" RELEASE_BASE="$RELEASE_WORK/base-image"
export RELEASE_CACHE_A="$RELEASE_WORK/cache-a" RELEASE_CACHE_B="$RELEASE_WORK/cache-b"
export GOTMPDIR="$HOME/.cache/go-tmp" TMPDIR="$HOME/.cache/go-tmp"
mkdir -p "$RELEASE_PRIVATE" "$RELEASE_SHARED" "$RELEASE_VERIFY_DIR" "$GOTMPDIR"
chmod 700 "$RELEASE_WORK" "$RELEASE_PRIVATE"
bash hack/release.sh prepare-signing
export PATH="$RELEASE_PRIVATE:$PATH"
```
`go version` must show the pinned version. `prepare-signing` downloads and checks `cosign` v3.1.1:
https://github.com/sigstore/cosign/releases/download/v3.1.1/cosign-linux-amd64 SHA256
`ae1ecd212663f3693ad9edf8b1a183900c9a52d3155ba6e354237f9a0f6463fc`; exit 0 means the tool is ready.
Create an empty-password key, pin its public half, and commit. Save it in encrypted offline custody.
After replacing the signing key, run upgrades with the new release's client;
an older client pins the earlier key and refuses the new release.
The pin command prints the public key hash and updated paths:
```sh
COSIGN_PASSWORD= cosign generate-key-pair --output-key-prefix "$RELEASE_WORK/release" </dev/null
sh hack/release-pin.sh "$RELEASE_WORK/release.pub"
git add internal/releaseverify/release-cosign.pub agent/skills/oberth/fetch-oberth
git commit -m "Pin release verification key"
```
Publish and test that pin commit on the public default branch, then check out its exact full ID.
The built commit must be that public pinned commit. These scripts create no tag. Follow the
[base publication procedure](../hack/base-image/README.md#publish-the-qualified-release-base).
Keep its public digest readback; fetch the verified base and build twice with independent caches:
```sh
git switch --detach "<public-pinned-commit>"
git rev-parse HEAD
bash hack/prepare-base.sh
make release-bytes
go build -p=2 -buildvcs=false -o "$RELEASE_TOOL" ./cmd/oberth-release
grep -E '"commit"|sha256:' "$RELEASE_DIR/release.json"
```
Base preparation exits 0 after checking its pin; the build prints “Release bytes built.” Compare
the printed record's commit with HEAD. Deliver the key and sign offline with closed input:

```sh
export OBERTH_SECRETS_DIR="$RELEASE_WORK/secrets"
mkdir -p "$OBERTH_SECRETS_DIR/oberth/data/release"
chmod 700 "$OBERTH_SECRETS_DIR" "$OBERTH_SECRETS_DIR/oberth/data/release"
cp "$RELEASE_WORK/release.key" "$OBERTH_SECRETS_DIR/oberth/data/release/signing"
chmod 600 "$OBERTH_SECRETS_DIR/oberth/data/release/signing"
bash hack/release.sh sign
"$RELEASE_TOOL" verify --dir "$RELEASE_DIR" --version "$VERSION"
```

Signing prints “Release signature verified.”; local verification prints “Release verified.” Log in
as `oauth2accesstoken` with a short-lived access token. Only where the cloud has no federation,
a service-account JSON key may use `_json_key`. For a workflow fallback, replace the identity
projection and login command with `secretstore exec --`, declare and grant
`oberth/data/release/registry/config.json` to the push step alone, and set `DOCKER_CONFIG` to its
delivered directory. Never fall back automatically after a federation failure.

```sh
export DOCKER_CONFIG="$RELEASE_PRIVATE/registry"
go tool -modfile=hack/tools/crane.mod crane auth login europe-west4-docker.pkg.dev \
  -u "<registry-user>" --password-stdin < "<registry-password-file>"
go tool -modfile=hack/tools/crane.mod crane push "$RELEASE_DIR/oberth.oci" \
  "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:$VERSION"
go tool -modfile=hack/tools/crane.mod crane push "$RELEASE_DIR/oberth-source-plugin.oci" \
  "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth-source-plugin:$VERSION"
"$RELEASE_TOOL" publish --dir "$RELEASE_DIR" --version "$VERSION" --origin "$RELEASE_ORIGIN" \
  --token-file "<github-token-file>"
bash hack/release.sh verify
"$RELEASE_TOOL" finalize --dir "$RELEASE_DIR" --version "$VERSION" --origin "$RELEASE_ORIGIN" \
  --token-file "<github-token-file>" --receipt "$RELEASE_SHARED/verified.json"
```

Compare each printed push digest with the record's images. Push the tag to GitHub first, at the
signed commit. Publish checks it, public image reads and every existing asset before writing,
uploads into a draft, confirms it and publishes it without making it latest; it prints the
version, file count and destination.
Clean verification prints “Release verified.” and writes the receipt. Finalize prints where `latest`
points; if it stays newer, nothing moved and exit 0 is intentional. Keep signed bytes for identical
retries; other bytes for a published asset refuse. Retain the receipt and build evidence.
By hand the token reaches the tool only as `--token-file <path>`: a file only you can read (mode
600) outside `$RELEASE_DIR`, else it refuses. Publish uploads only the ten signed file names.
After confirming encrypted offline key custody, delete both plaintext copies:

```sh
python3 -c 'import pathlib,sys; [pathlib.Path(p).unlink() for p in sys.argv[1:]]' \
  "$RELEASE_WORK/release.key" "$OBERTH_SECRETS_DIR/oberth/data/release/signing"
```

## Release accounts

Exact repo/file/step grants select deterministic release accounts for admitted release tags.
Branches and ordinary tags use CI accounts; YAML cannot choose release authority. Renames need new
grants. See [grant operations](operations.md#grants-and-secrets) and the
[release account-name rule](../internal/identityname/name.go).

## Verify downloaded bytes

Release files are the assets of `https://github.com/oberthci/oberth/releases/tag/<version>`.
`oberth-release <operation> --help` lists flags. Build from the trusted committed pin. Put the
checksum list, bundle, record and release files in a clean directory, then run:

```sh
oberth-release verify --dir <download-directory> --version <version>
```

Only the pinned key and signed digests decide trust. Versioned objects are create-only; the
[fetcher](../agent/skills/oberth/fetch-oberth) and installer verify before changing local state.
Clean verification checks public images and CLI version before issuing the `latest` receipt.
The workflow's 3,600-second deadline is a budget, not a measured time.
