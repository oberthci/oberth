#!/bin/sh
set -eu

# A normal `helm template -f old-values` merges today's chart defaults and
# cannot reproduce --reuse-values. Replace the chart's values.yaml as well to
# exercise the raw old-values path; the source checkout may be read-only.
chart=${1:-charts/oberth}
helm_tool=${HELM:-helm}
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
historical=$script_dir/testdata/chart-values-v0.17.53.yaml
utility_image=${2:-$(awk '$1 == "utilityImage:" { print $2 }' "$chart/values.yaml")}
test -n "$utility_image"
if grep -Eq '^utilityImage:' "$historical"; then
  echo 'historical values fixture must predate scratch-image settings' >&2
  exit 1
fi
work=$(mktemp -d)
trap 'find "$work" -delete' EXIT
server_image=example.invalid/oberth@sha256:1111111111111111111111111111111111111111111111111111111111111111
# The chart schema refuses the values the chart removed, as it must.
if "$helm_tool" template oberth "$chart" --namespace oberth --values "$historical" \
  --set-string "image.ref=$server_image" >/dev/null 2>"$work/error.txt"; then
  echo 'removed chart values rendered' >&2
  exit 1
fi
grep -Fq 'additional properties' "$work/error.txt"
# Every other historical value still renders: the upgrade refuses stored
# removed values, so a reused release no longer carries them.
fixture=$work/chart-values-v0.17.53.yaml
awk -f "$script_dir/strip-removed-chart-values.awk" "$historical" >"$fixture"
legacy_chart=$work/legacy-values-chart
cp -R -- "$chart" "$legacy_chart"
chmod -R u+w "$legacy_chart"
cp -- "$fixture" "$legacy_chart/values.yaml"
mirror=example.invalid/busybox@sha256:2222222222222222222222222222222222222222222222222222222222222222

check_default() {
  if ! grep -Fq -- "image: \"$utility_image\"" "$work/rendered.yaml"; then
    echo "upgrade render lacks pinned utility image $utility_image" >&2
    grep -n 'image:' "$work/rendered.yaml" >&2
    exit 1
  fi
  if grep -Eq '^[[:space:]]+image:[[:space:]]*(""|null)?[[:space:]]*$' "$work/rendered.yaml"; then
    echo 'upgrade rendered an empty container image' >&2
    exit 1
  fi
}

# Direct Helm --reuse-values compatibility: neither new key is present.
"$helm_tool" template oberth "$legacy_chart" --is-upgrade --namespace oberth \
  --set-string "image.ref=$server_image" >"$work/rendered.yaml"
check_default
# New policy defaults must also apply when old --reuse-values omit their keys.
grep -Fq 'kubernetes.io/metadata.name: "kube-system"' "$work/rendered.yaml"
grep -Fq 'name: oberth-pipeline-ingress' "$work/rendered.yaml"

# The reset-then-reuse path merges new defaults with the old release's values.
"$helm_tool" template oberth "$chart" --is-upgrade --namespace oberth \
  --values "$fixture" --set-string "image.ref=$server_image" >"$work/rendered.yaml"
check_default

# A previously stored utility mirror choice
# is retained before fresh --set overrides.
cp -- "$fixture" "$work/stored-values.yaml"
printf '\nutilityImage: %s\n' "$mirror" >>"$work/stored-values.yaml"
"$helm_tool" template oberth "$chart" --is-upgrade --namespace oberth \
  --values "$work/stored-values.yaml" --set-string "image.ref=$server_image" >"$work/rendered.yaml"
grep -Fq -- "image: \"$mirror\"" "$work/rendered.yaml"

# Operator mirrors retain precedence.
for source in "$legacy_chart" "$chart"; do
  "$helm_tool" template oberth "$source" --is-upgrade --namespace oberth \
    --values "$fixture" --set-string "image.ref=$server_image" \
    --set-string "utilityImage=$mirror" >"$work/rendered.yaml"
  grep -Fq -- "image: \"$mirror\"" "$work/rendered.yaml"
done
