#!/bin/sh
set -eu
source_root=$(pwd)
output=/qualification
mkdir -p "$output" "$output/tmp"
chmod 1777 "$output/tmp"
toolchain=/tmp/static-git-toolchain
/bin/sh hack/install-static-git-packages.sh "$source_root/internal/staticgit/build.env" "$toolchain"
export PATH="$toolchain/usr/bin:$toolchain/bin:$PATH"
export LD_LIBRARY_PATH="$toolchain/lib:$toolchain/usr/lib"
export CC="$toolchain/usr/bin/gcc --sysroot=$toolchain"
/bin/sh hack/build-static-git.sh "$source_root/internal/staticgit/build.env" "$source_root/hack/static-git-no-shell.patch" "$output" /tmp/static-git-source
# Same pinned static compiler as Git; this launcher is CI-only, not shipped.
# shellcheck disable=SC2086
$CC -std=c11 -O2 -Wall -Wextra -Werror -static hack/static-git-launcher.c -o "$output/launch-static-git"
unset CC LD_LIBRARY_PATH
arch=$(go env GOARCH)
export CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off GOENV=off
# These exact fresh binaries form the following allowed-runner qualification payload.
go build -trimpath -buildvcs=false -ldflags='-s -w -X main.version=ci' -o "$output/oberth" ./cmd/oberth
go test -c -tags=staticgit_qualification -o "$output/gitcache.test" ./internal/gitcache
go run ./hack/static-git-metadata --arch "$arch" --git "$output/git" \
  --apk-root "$toolchain" --patch "$source_root/hack/static-git-no-shell.patch" --sbom "$output/static-git-$arch.cdx.json"
go run ./hack/static-git-metadata --arch "$arch" --vulnerable-control --sbom "$output/static-git-control.cdx.json"
/bin/sh hack/scan-static-git.sh "$output" "$arch"
mkdir -p "$output/source/internal"
cp -R internal/gitcache "$output/source/internal/gitcache"
chmod 755 "$output/oberth" "$output/gitcache.test" "$output/git"

# Scratch-like payload: no runner tool or shell enters these directories.
rootfs=$output/rootfs
mkdir -p "$rootfs/usr/bin" "$rootfs/usr/local/bin" "$rootfs/etc/ssl/certs"
ln "$output/git" "$rootfs/usr/bin/git"
ln "$output/oberth" "$rootfs/usr/local/bin/oberth"
ln -s oberth "$rootfs/usr/local/bin/oberth-git-ssh"
cp "$toolchain/etc/ssl/certs/ca-certificates.crt" "$rootfs/etc/ssl/certs/ca-certificates.crt"
chmod 755 "$output/launch-static-git" "$rootfs/usr/bin/git" "$rootfs/usr/local/bin/oberth"
