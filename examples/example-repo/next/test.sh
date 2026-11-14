#!/bin/sh
set -eu
test "$(sh ./app.sh)" = 'hello from example-repo'
echo 'test: app passed'
test "$(cat expect.txt)" = fixed
echo 'test: source passed'
attempt=0
until test "$(wget -q -T 1 -O - "${1:-http://127.0.0.1:8080/}" 2>/dev/null)" = ready; do
  attempt=$((attempt + 1))
  test "$attempt" -lt 30 || { echo 'test: sidecar unavailable' >&2; exit 1; }
  sleep 1
done
echo 'test: sidecar passed'
