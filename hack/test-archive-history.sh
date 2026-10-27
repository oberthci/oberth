#!/bin/sh
# Offline binding gate: every file at all 878 commits of a real 3-pack history.
set -eu
OBERTH_ARCHIVE_PARITY_REPO=fixture go test -race -count=1 -timeout=10m \
  ./internal/gitcache -run 'TestArchiveRealHistory|TestCheckoutRealDeltaParity|TestStreamingDeltaBackwardCopies|TestDecodedObjectIntegrity|TestGoGitPatchProvenance'
