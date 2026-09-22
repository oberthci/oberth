#!/bin/sh
# Oberth CLI installer — https://oberth.ci
#
# Usage:
#   curl -fsSL https://oberth.ci/install.sh | sh
#   curl -fsSL https://oberth.ci/install.sh | sh -s -- --version v0.12.29
#
# What this script does:
#   1. Detects your OS (linux, darwin) and architecture (amd64, arm64)
#   2. Resolves the latest version (or uses the one you specify)
#   3. Downloads the binary and SHA256SUMS from releases.cloudtaser.io
#   4. Verifies the SHA-256 checksum before installing
#   5. If cosign is available, verifies the cosign signature
#   6. Installs to /usr/local/bin (or OBERTH_INSTALL_DIR)
#
# Environment variables:
#   OBERTH_INSTALL_DIR  Override install directory (default: /usr/local/bin)
#   OBERTH_VERSION      Pin a specific version (same as --version)
#
# URLs accessed (nothing else):
#   https://releases.cloudtaser.io/oberth/latest/VERSION
#   https://releases.cloudtaser.io/oberth/<version>/oberth-<os>-<arch>
#   https://releases.cloudtaser.io/oberth/<version>/SHA256SUMS
#   https://releases.cloudtaser.io/oberth/<version>/SHA256SUMS.sigstore.json
#
# The cosign public key is embedded in this script (not fetched from the same
# origin as the binary) so a compromised bucket alone cannot forge signatures.

set -eu

main() {

RELEASES="https://releases.cloudtaser.io/oberth"
INSTALL_DIR="${OBERTH_INSTALL_DIR:-/usr/local/bin}"

fail() { printf 'oberth-install: %s\n' "$@" >&2; exit 1; }

# ---- arguments ----

VERSION="${OBERTH_VERSION:-}"
while [ $# -gt 0 ]; do
  case "$1" in
    --version)
      [ $# -ge 2 ] || fail "--version requires a value (e.g. --version v0.12.29)"
      VERSION="$2"; shift 2 ;;
    --version=*)
      VERSION="${1#*=}"; shift ;;
    -h|--help)
      printf 'Oberth CLI installer\n\n'
      printf 'Usage:\n'
      printf '  curl -fsSL https://oberth.ci/install.sh | sh\n'
      printf '  curl -fsSL https://oberth.ci/install.sh | sh -s -- --version v0.12.29\n\n'
      printf 'Options:\n'
      printf '  --version VERSION  Install a specific version\n\n'
      printf 'Environment:\n'
      printf '  OBERTH_INSTALL_DIR  Install directory (default: /usr/local/bin)\n'
      printf '  OBERTH_VERSION      Pin a version (same as --version)\n'
      exit 0 ;;
    *) fail "unknown option: $1" ;;
  esac
done

# ---- detect platform ----

OS=$(uname -s)
case "$OS" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  *)      fail "unsupported OS: $OS" ;;
esac

ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *)             fail "unsupported architecture: $ARCH" ;;
esac

BINARY="oberth-${OS}-${ARCH}"

# ---- resolve version ----

if [ -z "$VERSION" ]; then
  VERSION=$(curl --proto '=https' --tlsv1.2 -fsSL "${RELEASES}/latest/VERSION") ||
    fail "could not resolve latest version"
fi

printf '\n  Oberth %s (%s/%s)\n\n' "$VERSION" "$OS" "$ARCH"

# ---- download ----

WORK=$(mktemp -d) && trap 'rm -rf "$WORK"' EXIT || fail "could not create temporary directory"

printf '  Downloading %s...' "$BINARY"
curl --proto '=https' --tlsv1.2 -fsSL "${RELEASES}/${VERSION}/${BINARY}" -o "${WORK}/${BINARY}" ||
  fail "download failed — check that ${VERSION} exists for ${OS}/${ARCH}"
printf ' done.\n'

printf '  Downloading SHA256SUMS...'
curl --proto '=https' --tlsv1.2 -fsSL "${RELEASES}/${VERSION}/SHA256SUMS" -o "${WORK}/SHA256SUMS" ||
  fail "could not download checksums"
printf ' done.\n'

# ---- verify checksum ----

printf '  Verifying checksum...'

EXPECTED=$(awk -v f="$BINARY" '$2 == f || $2 == "./"f {print $1}' "${WORK}/SHA256SUMS")
[ -n "$EXPECTED" ] || fail "no checksum found for ${BINARY} in SHA256SUMS"

if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL=$(sha256sum "${WORK}/${BINARY}" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL=$(shasum -a 256 "${WORK}/${BINARY}" | awk '{print $1}')
else
  fail "neither sha256sum nor shasum found — cannot verify download"
fi

if [ "$EXPECTED" != "$ACTUAL" ]; then
  printf ' FAILED.\n' >&2
  fail "checksum mismatch (expected ${EXPECTED}, got ${ACTUAL})"
fi

printf ' ok.\n'

# ---- verify signature (optional) ----

# Embedded cosign public key — does not come from the same origin as the
# binary, so a compromised R2 bucket alone cannot forge a valid signature.
# This key changes only on cosign key rotation (rare).
COSIGN_PUB='-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEORWOI3V47dAPvCBNec5aDeoGd0my
SxOZVlFj9e3OyEC2KD00sLeZoLKH4hewaY/+dmzYk1iuZXwNW56YmnNz2A==
-----END PUBLIC KEY-----'

if command -v cosign >/dev/null 2>&1; then
  printf '  Verifying signature...'
  printf '%s\n' "$COSIGN_PUB" > "${WORK}/cosign.pub"
  curl --proto '=https' --tlsv1.2 -fsSL "${RELEASES}/${VERSION}/SHA256SUMS.sigstore.json" -o "${WORK}/SHA256SUMS.sigstore.json" ||
    fail "could not download signature bundle"
  cosign verify-blob --key "${WORK}/cosign.pub" --insecure-ignore-tlog --bundle "${WORK}/SHA256SUMS.sigstore.json" "${WORK}/SHA256SUMS" ||
    fail "signature verification failed — the release may be tampered with"
  printf ' ok.\n'
else
  printf '  cosign not found — skipping signature verification (checksum verified)\n'
fi

# ---- install ----

[ -d "$INSTALL_DIR" ] || fail "${INSTALL_DIR} does not exist — set OBERTH_INSTALL_DIR to a valid directory"

if [ -w "$INSTALL_DIR" ]; then
  install -m 755 "${WORK}/${BINARY}" "${INSTALL_DIR}/oberth.tmp.$$"
  mv -f "${INSTALL_DIR}/oberth.tmp.$$" "${INSTALL_DIR}/oberth"
else
  printf '  Installing to %s (requires sudo)...\n' "$INSTALL_DIR"
  sudo install -m 755 "${WORK}/${BINARY}" "${INSTALL_DIR}/oberth.tmp.$$"
  sudo mv -f "${INSTALL_DIR}/oberth.tmp.$$" "${INSTALL_DIR}/oberth"
fi

printf '\n  Oberth %s installed to %s/oberth\n' "$VERSION" "$INSTALL_DIR"
printf '\n  Next step:\n    oberth setup\n\n'

}

main "$@"
