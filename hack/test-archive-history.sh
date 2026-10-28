#!/bin/sh
# Every file at all 878 commits of a real offline three-pack history.
set -eu
OBERTH_ARCHIVE_PARITY_REPO=fixture go test -race -count=1 -timeout=10m \
  ./internal/gitcache -run 'TestArchiveRealHistory|TestCheckoutRealDeltaParity'
