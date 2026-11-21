#!/bin/sh
set -eu
test "$(git rev-parse HEAD)" = "$EXPECTED_COMMIT"
printf 'walk: deliberate failure %s\n' "$EXPECTED_COMMIT"
exit 17
