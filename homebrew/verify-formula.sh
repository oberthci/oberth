#!/bin/sh
# Homebrew formula verification: prove the formula is structurally sound,
# internally version-consistent, and that declared SHA256 digests match the
# published release artifacts on releases.cloudtaser.io.
#
# Phase 1 (structural) runs offline with POSIX tools only.
# Phase 2 (digest) downloads each platform binary and compares the actual
# SHA256 against the formula's declared digest. This is the independent
# cross-check: the release burn computes digests at build time against its
# own SHA256SUMS; this script re-derives them from the published artifacts.
set -eu

cd "$(dirname "$0")"

fail() {
  echo "verify-formula: FAIL: $*" >&2
  exit 1
}

download() {
  _url=$1; _dest=$2
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$_dest" "$_url"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$_dest" "$_url"
  else
    fail "neither curl nor wget is available for digest verification"
  fi
}

formula=Formula/oberth.rb
test -f "$formula" || fail "$formula is missing"
test ! -L "$formula" || fail "$formula must be a regular file"

# --- Phase 1: structural checks (offline) ---

version=$(sed -n 's/^[[:space:]]*version[[:space:]]*"\([0-9][0-9.]*\)".*$/\1/p' "$formula" | head -1)
[ -n "$version" ] || fail "formula declares no version"

for platform in darwin-amd64 darwin-arm64 linux-amd64 linux-arm64; do
  grep -q "releases.cloudtaser.io/oberth/v$version/oberth-$platform" "$formula" || \
    fail "formula lacks the v$version $platform artifact URL"
done

sha_count=$(grep -c '^[[:space:]]*sha256[[:space:]]*"[0-9a-f]\{64\}"' "$formula") || sha_count=0
[ "$sha_count" -eq 4 ] || fail "formula must pin exactly 4 sha256 digests, found $sha_count"

if grep -Eq 'http://' "$formula"; then
  fail "formula must not reference plain http URLs"
fi

echo "phase 1 OK: version $version, 4 platforms, 4 digests, structure valid"

# --- Phase 2: digest verification against published artifacts ---

command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required for digest verification"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

verified=0
for platform in darwin-amd64 darwin-arm64 linux-amd64 linux-arm64; do
  url="https://releases.cloudtaser.io/oberth/v$version/oberth-$platform"

  # Extract the sha256 on the line immediately following this platform's url.
  expected=$(sed -n "/releases\.cloudtaser\.io\/oberth\/v${version}\/oberth-${platform}/{n;s/.*\"\([0-9a-f]\{64\}\)\".*/\1/p;}" "$formula")
  [ -n "$expected" ] || fail "could not extract sha256 for $platform from formula"

  printf '  %s: downloading ... ' "$platform"
  if ! download "$url" "$tmpdir/$platform"; then
    fail "download failed: $url"
  fi

  actual=$(sha256sum "$tmpdir/$platform" | cut -d' ' -f1)
  if [ "$actual" != "$expected" ]; then
    fail "$platform digest mismatch: formula=$expected actual=$actual"
  fi

  printf 'verified\n'
  verified=$((verified + 1))
  rm -f "$tmpdir/$platform"
done

[ "$verified" -eq 4 ] || fail "verified only $verified/4 platform digests"
echo "phase 2 OK: all 4 digests match published artifacts"
echo "formula OK: v$version fully verified"
