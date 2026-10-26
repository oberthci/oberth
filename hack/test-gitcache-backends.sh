#!/bin/sh
# Run the same cache fixtures against each selected backend, without changing
# the Git CLI used by test clients/upstream fixtures. Extra go test flags may
# be supplied with the standard GOFLAGS environment variable.
set -eu
if [ "$#" -eq 0 ]; then
  set -- cli go-git
fi
for backend in "$@"; do
  printf 'gitcache parity backend=%s\n' "$backend"
  OBERTH_TEST_GIT_BACKEND="$backend" go test -race -count=1 ./internal/gitcache
done
