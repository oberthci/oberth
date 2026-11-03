#!/bin/sh
# The source mount is read-only; untt creates a snapshot directory beside tests.
set -eu
workspace=$(mktemp -d /tmp/oberth-chart-unittest.XXXXXXXX)
trap 'find "$workspace" -depth -delete' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cp -R charts/oberth "$workspace/oberth"
/tmp/oberth-tools/bin/untt-linux-amd64 --strict "$workspace/oberth"
