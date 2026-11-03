#!/bin/sh
# Produce the immutable build context. An authorized release publisher passes
# it to its image builder with --build-arg QEMU_RUNTIME=<reviewed image@sha256>.
set -eu
: "${QEMU_RUNTIME:?a reviewed digest-pinned linux/amd64 QEMU runtime is required}"
case "$QEMU_RUNTIME" in *@sha256:*) ;; *) echo 'QEMU_RUNTIME must use @sha256' >&2; exit 1 ;; esac
digest=${QEMU_RUNTIME##*@sha256:}
[ "${#digest}" = 64 ] || exit 1
case "$digest" in *[!0-9a-f]*) exit 1 ;; esac
[ "$#" = 1 ] || { echo 'usage: packaging/kvm-harness/build.sh OUTPUT_DIRECTORY' >&2; exit 1; }
mkdir -p "$1"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o "$1/oberth-kvm-harness" ./cmd/oberth-kvm-harness
cp packaging/kvm-harness/Dockerfile "$1/Dockerfile"
printf '%s\n' "$QEMU_RUNTIME" > "$1/QEMU_RUNTIME"
sha256sum "$1/oberth-kvm-harness"
