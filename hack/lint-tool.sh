#!/usr/bin/env bash
set -Eeuo pipefail
exec go tool -modfile="$(dirname "$0")/tools/go.mod" golangci-lint "$@"
