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
unset CC LD_LIBRARY_PATH
arch=$(go env GOARCH)
export CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off GOENV=off
# These exact fresh binaries are mounted into the following scratch-image Pod.
go build -trimpath -buildvcs=false -ldflags='-s -w -X main.version=ci' -o "$output/oberth" ./cmd/oberth
go test -c -tags=staticgit_qualification -o "$output/gitcache.test" ./internal/gitcache
go run ./hack/static-git-metadata --arch "$arch" --git "$output/git" \
  --apk-root "$toolchain" --patch "$source_root/hack/static-git-no-shell.patch" --sbom "$output/static-git-$arch.cdx.json"
go run ./hack/static-git-metadata --arch "$arch" --vulnerable-control --sbom "$output/static-git-control.cdx.json"
/bin/sh hack/scan-static-git.sh "$output" "$arch"
mkdir -p "$output/source/internal"
cp -R internal/gitcache "$output/source/internal/gitcache"
chmod 755 "$output/oberth" "$output/gitcache.test" "$output/git"
