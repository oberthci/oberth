#!/bin/sh
set -eu
cd /work/src
test "$(git rev-parse HEAD)" = "$EXPECTED_COMMIT"
test -z "$(git status --porcelain=v1 --untracked-files=all)"
CGO_ENABLED=0 go build -p=2 -buildvcs=true -o /tmp/walk-app .
/tmp/walk-app
python3 ./publish.py
ref=$(cat /tmp/image-ref)
# The helper's private files come from the approved, exact workflow/step grant.
COSIGN_PASSWORD=$(cat /run/private-files/owned/walk/signing_password)
export COSIGN_PASSWORD
cosign sign --yes --signing-config /work/src/signing-config.json --allow-http-registry --key /run/private-files/owned/walk/signing_key "$ref" >/tmp/sign.out 2>/tmp/sign.err
cosign verify --insecure-ignore-tlog --allow-http-registry --key ./signing.pub "$ref" >/tmp/verify.json 2>/tmp/verify.err
test -s /tmp/verify.json
printf 'walk: signed and verified %s\nwalk: release %s\n' "$ref" "$RELEASE_TAG"
