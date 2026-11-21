#!/bin/sh
set -eu
test "$(git rev-parse HEAD)" = "$EXPECTED_COMMIT"
test -z "$(git status --porcelain)"
test "$(cat source-marker)" = "$EXPECTED_MARKER"
printf 'walk: restart source %s clean\n' "$EXPECTED_COMMIT"
