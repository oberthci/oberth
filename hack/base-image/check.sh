#!/usr/bin/env bash
# Check a locally prepared OCI layout without a daemon or a registry.
set -Eeuo pipefail
cd "$(dirname "$0")/../.."
export GOTMPDIR=${GOTMPDIR:-$HOME/.cache/go-tmp}
export TMPDIR=$GOTMPDIR
mkdir -p "$GOTMPDIR"
scratch=$(mktemp -d "$GOTMPDIR/base-check.XXXXXX")
trap 'find "$scratch" -depth -delete' EXIT
go build -buildvcs=false -o "$scratch/oberth-release" ./cmd/oberth-release
"$scratch/oberth-release" base "$@"
