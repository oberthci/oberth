#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
refuse_source() {
    printf '%s\n' 'Release Git metadata is unavailable; build from a clean checkout.' >&2
    exit 1
}
commit=$(git rev-parse --verify HEAD 2>/dev/null) || refuse_source
source_date=$(git -c log.showSignature=false show -s --format=%cI "$commit" 2>/dev/null) || refuse_source
source_status=$(git status --porcelain 2>/dev/null) || refuse_source
export GOTOOLCHAIN=local
export GOTMPDIR=${GOTMPDIR:-$HOME/.cache/go-tmp}
export TMPDIR=$GOTMPDIR
mkdir -p "$GOTMPDIR"
mode=${1:?Use once or reproduce}
[[ $mode = once || $mode = reproduce ]] || { echo 'Build mode is invalid; use once or reproduce.' >&2; exit 1; }
version=${VERSION:-}
output=${RELEASE_DIR:-dist}
base=${RELEASE_BASE:-base-image}
version_pattern=$(cat internal/workflowpolicy/release-version.pattern) || { echo 'Release version rule is missing; use the complete admitted source.' >&2; exit 1; }
[[ $version =~ $version_pattern ]] || { echo 'Release version is invalid; use vMAJOR.MINOR.PATCH.' >&2; exit 1; }
[[ ! -e $output ]] || { echo 'Release output exists; choose an empty output path.' >&2; exit 1; }
[[ -z $source_status ]] || { echo 'Release tree is dirty; commit or remove local changes first.' >&2; exit 1; }
pinned=$(cat .oberth/pins/go-toolchain 2>/dev/null || true)
found=$(go env GOVERSION 2>/dev/null) || found=unavailable
if [[ -z $pinned || $found != "$pinned" ]]; then
    printf '%s\n' 'Go toolchain differs; use the version in .oberth/pins/go-toolchain.' >&2
    printf 'Found Go: %s\nRequired Go: %s\n' "${found:-unavailable}" "${pinned:-missing .oberth/pins/go-toolchain}" >&2
    exit 1
fi
if [[ $mode = reproduce && ( -z ${RELEASE_CACHE_A:-} || -z ${RELEASE_CACHE_B:-} ) ]]; then
    echo 'Release caches are missing; set RELEASE_CACHE_A and RELEASE_CACHE_B.' >&2
    exit 1
fi
for path in cmd/oberth/main.go cmd/oberth-source-plugin/main.go charts/oberth/Chart.yaml "$base/index.json" .oberth/pins/base-image.digest; do
    [[ -f $path ]] || { echo 'Release input is missing; provide the commands, chart and pinned base.' >&2; exit 1; }
done
scratch=$(mktemp -d "$GOTMPDIR/release-bytes.XXXXXX")
trap 'find "$scratch" -depth -delete' EXIT
export CGO_ENABLED=0
export GOFLAGS="${GOFLAGS:-} -mod=readonly -buildvcs=false -trimpath"
go mod verify
go build -o "$scratch/pack" ./cmd/oberth-release
passes=(first)
[[ $mode = once ]] || passes+=(second)
for pass in "${passes[@]}"; do
    mkdir -p "$scratch/$pass"
    if [[ $mode = reproduce ]]; then
        if [[ $pass = first ]]; then export GOCACHE=$RELEASE_CACHE_A
        else export GOCACHE=$RELEASE_CACHE_B; fi
    fi
    for target in darwin-amd64 darwin-arm64 linux-amd64 linux-arm64; do
        GOOS=${target%-*} GOARCH=${target#*-} go build -ldflags "-buildid= -X main.version=$version -X main.commit=$commit -X main.date=$source_date" -o "$scratch/$pass/oberth-$target" ./cmd/oberth
    done
    for arch in amd64 arm64; do
        GOOS=linux GOARCH=$arch go build -ldflags=-buildid= -o "$scratch/$pass/oberth-source-plugin-linux-$arch" ./cmd/oberth-source-plugin
    done
    "$scratch/pack" package --dir "$scratch/$pass" --base "$base" --version "$version" --commit "$commit"
done
if [[ $mode = reproduce ]]; then
    cmp -s "$scratch/first/SHA256SUMS" "$scratch/second/SHA256SUMS" || { echo 'Release builds differ; inspect the build inputs and toolchain.' >&2; exit 1; }
fi
mv "$scratch/first" "$output"
echo 'Release bytes built.'
