# Release a tested commit

## Release your project

As administrator, provision [pipeline grants](pipelines.md#secrets-and-identity) and commit
your publisher in the tested `.oberth/release.yaml`. Promote a full commit ID from any directory:
```sh
oberth promote <tested-commit-sha> --repo <repository>
```

Promotion may create a different default-branch commit without moving local `HEAD`. Save local work,
then fetch and check out the full resulting commit ID printed by promotion:

```sh
git fetch -- oberth refs/heads/<default-branch>
git switch --detach <resulting-commit-sha>
```

Wait for its passing, unassisted CI. If default moved again, inspect its head and evidence first:

```sh
oberth release v0.1.0
```

Release checks eligibility and grants before creating/pushing a tag; refusals create none. Follow
printed grant remedies and retry. Existing tags require the same commit. Confirm the run and
[publication receipt](operations.md#retention-and-publication).

`check --local` with a release filename selects no release run or release-tier grants. No release
dry-run flag exists: use `release` when ready to create the tag.

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

This is the normal path, as for any repository: push a branch, wait for its green CI, promote, wait
for the promotion to deliver, then release a version strictly above `latest`:

```sh
oberth promote <tested-commit-sha>
oberth release <version-above-latest>
```

The release pipeline prepares the base and signer, builds, signs, pushes both images to the
registry, publishes the files as the tag's GitHub release, verifies them in a clean Pod and advances
`latest`. The build checks its commit without rerunning tests. Only signing gets the long-lived key,
only the image push the registry login, only publish/finalize the GitHub token. Branch CI reaches
neither those credentials nor the release cache. Oberth puts the tag on GitHub when it admits the
release, before the run; that uses the version: rerun a failed run or release the next one.
Only an administrator may rerun a release run; agents can release the next version.

Existing installations must add `release-publish: "1"` to `oberth-sync-limits` before the first
release from this main. Edit [resources](../.oberth/pipeline-resources.yaml): storage, positive `go`
limit, HTTPS origin; apply in the pipeline namespace. Caches need uid/gid 65534 access. Then grant:

```sh
oberth access allow --kind pvc --tier ci '<repo>' 'build-cache'
oberth access allow --kind pvc --tier release '<repo>' 'release-cache'
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
The release needs exactly three secrets:

- `oberth/data/release/registry/config.json` (step `release-images-pushed`): a Docker
  `config.json` logged in to `europe-west4-docker.pkg.dev` as `_json_key` with a service-account
  key (an access token expires within the hour); the by-hand `crane auth login` line writes it.
- `oberth/data/release/signing` (step `release-files-signed`): the pinned private `release.key`.
- `oberth/data/release/github-token` (steps `release-published-immutable` and
  `latest-advanced-monotonic`): a token with read/write contents on this repository only.

```sh
oberth secret put --repo '<repo>' --file .oberth/release.yaml --step release-images-pushed \
  oberth/data/release/registry/config.json < <registry-login-file>
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
[walk-vm.yaml](../hack/walk-vm.yaml), gates nothing: no run selects it. Its own objects are in
[walk-vm-resources.yaml](../hack/walk-vm-resources.yaml), with its VM capability line.

## First release by hand

The fallback, and the bootstrap that pins the signing key. Use Linux amd64, Go **go1.26.10**,
Python 3, curl, Git, make, grep and `sha256sum` in a clean tree.
Have registry write access and a GitHub token file allowed to write this repository's releases.
Allow about 12 GiB and eight minutes for the byte build; cold caches can take longer.
Keep work and credentials outside Git; staging needs 0.7 GiB in `$TMPDIR`.
Choose a stable `vMAJOR.MINOR.PATCH` strictly above `latest` (404 means no floor):

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
as `oauth2accesstoken` with a short-lived access token or `_json_key` with a service-account key:

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

Admission chooses each credential-bearing step's account from its secret grants. A
server-classified release tag in default history uses deterministic release-step accounts; branches
and ordinary tags use CI accounts. Author parameters and account names cannot select release
authority. Exact-step grants align secrets on repository/file/step accounts; wildcards share one.
Submission creates release accounts; pipeline and executor accounts stay separate. Renames need
new grants; see [grant operations](operations.md#grants-and-secrets) and the
[release account-name rule](../internal/identityname/name.go).

## Verify downloaded bytes

Release files are the assets of `https://github.com/oberthci/oberth/releases/tag/<version>`.
`oberth-release <operation> --help` lists flags. Build from the trusted committed pin. Put the
checksum list, bundle, record and release files in a clean directory, then run:

```sh
oberth-release verify --dir <download-directory> --version <version>
```

Where the files came from never decides trust; only the pinned key and the signed digests do.
Signed `SHA256SUMS` and `release.json` bind shipped digests; versioned objects are create-only. The
[fetcher](../agent/skills/oberth/fetch-oberth) and installer verify before changing local state.

Local checks are not publication proof. Clean verification checks public images and CLI version to
issue the `latest` receipt. The workflow's 3,600-second deadline is a budget, not a measured time.
