#!/bin/sh
# Validates the prepare-caches init container spec in the rendered Oberth
# chart template.
#
# The former prepare-node.sh lived in charts/oberth/files/ and was tested as
# a standalone script here. That file was intentionally removed; the inline
# prepare-caches init container in the deployment template replaced it (see
# deployment.yaml comment: "this container replaces the former manual
# prepare-node.sh step"). This test validates the rendered init container
# carries the required safety properties:
#
#   1. Runs as root (UID 0) to assign ownership
#   2. Targets the nonroot service user (65534:65534) with mode 0750
#   3. Drops all capabilities except CHOWN, DAC_OVERRIDE, FOWNER
#   4. Refuses symlinks at the target path
#   5. Refuses non-directory files at the target path
#   6. readOnlyRootFilesystem and no privilege escalation
#   7. Receives both cache root paths as args
set -eu

chart=${1:-charts/oberth}
server_image=example.invalid/oberth@sha256:1111111111111111111111111111111111111111111111111111111111111111

manifest=$(mktemp)
trap 'rm -f "$manifest"' EXIT

helm template oberth "$chart" \
  --set "image.ref=$server_image" \
  --show-only templates/deployment.yaml >"$manifest"

# Extract the prepare-caches init container block (from its name to the
# next container or the containers: key).
init=$(sed -n '/name: prepare-caches/,/^      containers:/p' "$manifest")

if [ -z "$init" ]; then
  echo "FAIL: prepare-caches init container not found in rendered deployment" >&2
  exit 1
fi

# 1. Runs as root (UID 0) — required for chown.
echo "$init" | grep -q 'runAsUser: 0'

# 2. Script creates directories owned by 65534:65534 with mode 0750.
echo "$init" | grep -q 'install -d -m 0750 -o 65534 -g 65534'

# 3. Drops ALL capabilities, adds only the three needed for mkdir+chown.
echo "$init" | grep -q 'drop:'
echo "$init" | grep -qF '"ALL"'
echo "$init" | grep -qF '"CHOWN"'
echo "$init" | grep -qF '"DAC_OVERRIDE"'
echo "$init" | grep -qF '"FOWNER"'

# 4. Refuses symlinks at the target path.
echo "$init" | grep -q 'refusing symlink'

# 5. Refuses non-directory files at the target path.
echo "$init" | grep -q 'not a directory'

# 6. Hardened security context.
echo "$init" | grep -q 'readOnlyRootFilesystem: true'
echo "$init" | grep -q 'allowPrivilegeEscalation: false'

# 7. Both cache roots appear as args (default values from chart).
echo "$init" | grep -qF '/var/cache/oberth/ci'
echo "$init" | grep -qF '/var/cache/oberth/release'
