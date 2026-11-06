#!/bin/sh
# The source mount is read-only; untt creates a snapshot directory beside tests.
set -eu
unittest_bin=${HELM_UNITTEST_BIN:-untt-linux-amd64}
if ! command -v "$unittest_bin" >/dev/null 2>&1; then
  printf 'Helm unittest binary not found: %s; install it on PATH or set HELM_UNITTEST_BIN to its executable path.\n' "$unittest_bin" >&2
  exit 127
fi
workspace=$(mktemp -d /tmp/oberth-chart-unittest.XXXXXXXX)
trap 'find "$workspace" -depth -delete' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cp -R charts/oberth "$workspace/oberth"
"$unittest_bin" --strict "$workspace/oberth"
