#!/bin/sh
set -eu
cd /work/src
test "$(git rev-parse HEAD)" = "$EXPECTED_COMMIT"
test -z "$(git status --porcelain=v1 --untracked-files=all)"
test "$(cat source-marker)" = "$EXPECTED_MARKER"
printf 'walk: live %s\n' "$EXPECTED_COMMIT"
printf 'walk: source %s clean\n' "$EXPECTED_COMMIT"
CGO_ENABLED=0 go build -p=2 -buildvcs=true -o /tmp/walk-app .
/tmp/walk-app
test -z "$(git status --porcelain=v1 --untracked-files=all)"
printf 'walk: build passed\n'
