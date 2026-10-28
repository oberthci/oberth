#!/bin/sh
set -eu
# This exact package closure is shared by Dockerfile and the unprivileged CI Pod.
# It adds build tools inside that Pod/image, never to its Kubernetes node.
pins=${1:-internal/staticgit/build.env}
# shellcheck disable=SC1090
. "$pins"
# Intentional word splitting of a source-controlled, version-qualified closure.
# shellcheck disable=SC2086
if [ "$#" -eq 2 ]; then
  # CI has a read-only root filesystem. Install the exact toolchain under its
  # private /tmp; no package script, chroot, host mount or capability is needed.
  # shellcheck disable=SC2086
  apk --root "$2" --initdb --no-scripts --keys-dir /etc/apk/keys \
    --repositories-file /etc/apk/repositories add --no-cache $APK_PACKAGES
else
  # shellcheck disable=SC2086
  apk add --no-cache $APK_PACKAGES
fi
