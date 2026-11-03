#!/usr/bin/env bash
set -euo pipefail
export CGO_ENABLED=0
export GOMAXPROCS=2
export GOFLAGS="${GOFLAGS:-} -p=2"
# Include maintenance/build-tool mains in hack, not only shipped cmd binaries.
deadcode -json ./... | python3 -I hack/check-deadcode.py hack/deadcode-allowlist.json
