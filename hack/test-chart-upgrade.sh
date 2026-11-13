#!/bin/sh
set -eu
chart=${1:-charts/oberth}
helm_tool=${HELM:-helm}
fixtures=$(dirname "$0")/../internal/installer/testdata
work=$(mktemp -d)
trap 'find "$work" -delete' EXIT
server_image=example.invalid/oberth@sha256:1111111111111111111111111111111111111111111111111111111111111111
if "$helm_tool" template oberth "$chart" --is-upgrade --namespace oberth \
  --values "$fixtures/v0.22.3-values.yaml.golden" --set-string "image.ref=$server_image" >"$work/raw.yaml" 2>&1; then
  echo 'historical Transit values unexpectedly accepted by the closed schema' >&2
  exit 1
fi
grep -q 'transit' "$work/raw.yaml"
"$helm_tool" template oberth "$chart" --is-upgrade --namespace oberth \
  --values "$fixtures/v0.22.3-cleaned-values.yaml.golden" \
  --set-string "image.ref=$server_image" >"$work/upgrade.yaml"
grep -Fxq '      priorityClassName: oberth-oberth-control-plane' "$work/upgrade.yaml"
test "$(grep -c '^kind: PriorityClass$' "$work/upgrade.yaml")" -eq 1
if "$helm_tool" template oberth "$chart" --is-upgrade --set admission.capacity=3 >"$work/invalid.yaml" 2>&1; then
  echo 'unknown chart value accepted' >&2
  exit 1
fi
grep -q 'additional properties' "$work/invalid.yaml"
echo 'v0.22.3 historical values render after exact installer-owned Transit cleanup'
