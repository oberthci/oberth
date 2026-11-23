#!/usr/bin/env bash
set -Eeuo pipefail
exec env GOWORK=off GOFLAGS='-mod=readonly -p=2 -buildvcs=false' go tool -modfile="$(dirname "$0")/tools/go.mod" golangci-lint "$@"
