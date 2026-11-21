#!/usr/bin/env bash
# Assemble a local disk from a reviewed guest image; never start or publish it.
set -Eeuo pipefail
: "${WALK_BASE_IMAGE:?Provide the reviewed local guest disk}"
: "${WALK_BASE_SHA256:?Provide its reviewed SHA-256}"
: "${WALK_OUTPUT_IMAGE:?Choose a new output disk path}"
printf '%s  %s\n' "$WALK_BASE_SHA256" "$WALK_BASE_IMAGE" | sha256sum -c -
[[ ! -e $WALK_OUTPUT_IMAGE ]] || { echo 'VM output exists; choose a new disk path.' >&2; exit 1; }
cp --reflink=auto "$WALK_BASE_IMAGE" "$WALK_OUTPUT_IMAGE"
sha256sum "$WALK_OUTPUT_IMAGE"
