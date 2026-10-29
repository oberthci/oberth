#!/bin/sh
set -eu
# This exact package closure is shared by Dockerfile and the unprivileged CI Pod.
# It adds build tools inside that Pod/image, never to its Kubernetes node.
pins=${1:-internal/staticgit/build.env}
# shellcheck disable=SC1090
. "$pins"
# The server replaces the template PATH and omits /sbin. Resolve no package
# manager through PATH: the immutable Alpine builder supplies this exact one.
apk_pin=
for package in $APK_PACKAGES; do
  case "$package" in
    apk-tools=*)
      [ -z "$apk_pin" ] || { echo 'static Git packages: duplicate apk-tools pin' >&2; exit 1; }
      apk_pin=${package#apk-tools=}
      ;;
  esac
done
[ -n "$apk_pin" ] || { echo 'static Git packages: missing apk-tools pin' >&2; exit 1; }
if [ ! -f /sbin/apk ] || [ ! -x /sbin/apk ] || [ -L /sbin/apk ]; then
  echo 'static Git packages: expected a regular executable /sbin/apk in the pinned Alpine builder' >&2
  exit 1
fi
apk_version=$(/sbin/apk --version) || {
  echo 'static Git packages: /sbin/apk version check failed' >&2
  exit 1
}
case "$apk_version" in
  "apk-tools $apk_pin, compiled for x86_64."|"apk-tools $apk_pin, compiled for aarch64.") ;;
  *) echo "static Git packages: /sbin/apk does not match apk-tools=$apk_pin" >&2; exit 1 ;;
esac
# Intentional word splitting of a source-controlled, version-qualified closure.
# shellcheck disable=SC2086
if [ "$#" -eq 2 ]; then
  # CI has a read-only root filesystem. Install the exact toolchain under its
  # private /tmp; no package script, chroot, host mount or capability is needed.
  # shellcheck disable=SC2086
  /sbin/apk --root "$2" --initdb --no-scripts --keys-dir /etc/apk/keys \
    --repositories-file /etc/apk/repositories add --no-cache $APK_PACKAGES
else
  # shellcheck disable=SC2086
  /sbin/apk add --no-cache $APK_PACKAGES
fi
