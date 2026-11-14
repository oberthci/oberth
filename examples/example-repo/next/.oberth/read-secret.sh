#!/bin/sh
set -eu
umask 077
# The client and all login/scratch volumes belong to this granted step.
cleanup() {
  if test -s /private/session; then
    BAO_TOKEN="$(cat /private/session)" bao token revoke -self >/dev/null 2>&1 || :
  fi
  rm -f /private/session /private/marker
}
trap cleanup EXIT
bao write -field=token auth/kubernetes/login   role="$STORE_ROLE" jwt=@/var/run/secrets/secretstore/token > /private/session
BAO_TOKEN="$(cat /private/session)" bao kv get -field=marker   oberth/release/example-repo > /private/marker
test -s /private/marker
echo 'secret: marker present'
printf 'release: %s\n' "$1"
