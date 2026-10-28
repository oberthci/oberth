#!/bin/sh
set -eu
source_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck disable=SC1091
. "$source_root/internal/staticgit/build.env"
input_dir=$1
shift
test "$#" -gt 0
case "$(uname -m)" in
  x86_64) scanner_arch=amd64; scanner_sha=$GRYPE_AMD64_SHA256 ;;
  aarch64) scanner_arch=arm64; scanner_sha=$GRYPE_ARM64_SHA256 ;;
  *) echo 'unsupported static scanner architecture' >&2; exit 1 ;;
esac
scratch=$(mktemp -d "${TMPDIR:-/tmp}/oberth-static-scan.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
archive=$scratch/grype.tar.gz
url=https://github.com/anchore/grype/releases/download/v$GRYPE_VERSION/grype_${GRYPE_VERSION}_linux_${scanner_arch}.tar.gz
if command -v curl >/dev/null 2>&1; then
  curl --fail --silent --show-error --location --proto '=https' --max-time 120 "$url" --output "$archive"
else
  wget -q -T 120 -O "$archive" "$url"
fi
printf '%s  %s\n' "$scanner_sha" "$archive" | sha256sum -c -
tar -xzf "$archive" -C "$scratch" grype
go build -trimpath -buildvcs=false -o "$scratch/scanner-driver" "$source_root/hack/static-git-scanner"
if "$scratch/scanner-driver" "$scratch/grype" "$source_root/hack/static-git-grype.yaml" \
  "$scratch/db" "$input_dir/static-git-control.cdx.json" "alpine:$ALPINE_VERSION" \
  > "$input_dir/static-git-control.scan.json"; then
  echo 'static Git vulnerability coverage control unexpectedly passed' >&2
  exit 1
else
  scanner_rc=$?
  test "$scanner_rc" -eq 2
fi
for artifact_arch do
  "$scratch/scanner-driver" "$scratch/grype" "$source_root/hack/static-git-grype.yaml" \
    "$scratch/db" "$input_dir/static-git-$artifact_arch.cdx.json" "alpine:$ALPINE_VERSION" \
    > "$input_dir/static-git-$artifact_arch.scan.json"
  go run "$source_root/hack/static-git-scan-receipt" --arch "$artifact_arch" \
    --sbom "$input_dir/static-git-$artifact_arch.cdx.json" \
    --scan "$input_dir/static-git-$artifact_arch.scan.json" \
    --control "$input_dir/static-git-control.scan.json" \
    > "$input_dir/static-git-$artifact_arch.security.json"
done
