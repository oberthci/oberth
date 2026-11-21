#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=local
export GOTMPDIR=${GOTMPDIR:-$HOME/.cache/go-tmp}
export TMPDIR=$GOTMPDIR
mkdir -p "$GOTMPDIR"
repository=$(cat .oberth/pins/base-image.repository)
pin=$(cat .oberth/pins/base-image.digest)
base=${RELEASE_BASE:?"Set RELEASE_BASE to this run's work volume"}
[[ -n $repository && $pin =~ ^sha256:[0-9a-f]{64}$ ]] || { echo 'Release base pin is invalid; review its repository and digest.' >&2; exit 1; }
[[ ! -e $base ]] || { echo 'Release base output exists; use a fresh run work volume.' >&2; exit 1; }
mkdir -p "$(dirname "$base")"
stage=$(mktemp -d "$(dirname "$base")/.base-image.XXXXXX")
trap 'find "$stage" -depth -delete' EXIT
if ! go tool -modfile=hack/tools/crane.mod crane pull --format=oci "$repository@$pin" "$stage"; then
    echo 'Release base fetch failed; publish the pin or check registry access.' >&2
    printf 'Repository: %s\nDigest: %s\n' "$repository" "$pin" >&2
    exit 1
fi
index="$stage/blobs/sha256/${pin#sha256:}"
[[ -f $index ]] || { echo 'Release base index is missing; publish the reviewed index at its pinned digest.' >&2; exit 1; }
cp "$index" "$stage/index.json"
bash hack/base-image/check.sh --base "$stage" --pin .oberth/pins/base-image.digest
mv "$stage" "$base"
trap - EXIT
echo 'Release base prepared and verified.'
