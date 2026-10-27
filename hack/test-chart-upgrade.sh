#!/bin/sh
set -eu

# A normal `helm template -f old-values` merges today's chart defaults and
# cannot reproduce --reuse-values. Replace the chart's values.yaml as well to
# exercise the raw old-values path; the source checkout may be read-only.
chart=${1:-charts/oberth}
helm_tool=${HELM:-helm}
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
fixture=$script_dir/testdata/chart-values-v0.17.53.yaml
utility_image=${2:-$(awk '$1 == "utilityImage:" { print $2 }' "$chart/values.yaml")}
test -n "$utility_image"
if grep -Eq '^(utilityImage|gitBackend):' "$fixture"; then
  echo 'historical values fixture must predate scratch-image settings' >&2
  exit 1
fi
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
legacy_chart=$work/legacy-values-chart
cp -R -- "$chart" "$legacy_chart"
chmod -R u+w "$legacy_chart"
cp -- "$fixture" "$legacy_chart/values.yaml"
server_image=example.invalid/oberth@sha256:1111111111111111111111111111111111111111111111111111111111111111
mirror=example.invalid/busybox@sha256:2222222222222222222222222222222222222222222222222222222222222222

check_default() {
  if ! grep -Fq -- "image: \"$utility_image\"" "$work/rendered.yaml"; then
    echo "upgrade render lacks pinned utility image $utility_image" >&2
    grep -n 'image:' "$work/rendered.yaml" >&2
    exit 1
  fi
  grep -Fq -- '--git-backend=cli' "$work/rendered.yaml"
  grep -A1 -F 'name: OBERTH_GITCACHE_SHADOW' "$work/rendered.yaml" | grep -Fq 'value: "0"'
  if grep -Eq '^[[:space:]]+image:[[:space:]]*(""|null)?[[:space:]]*$' "$work/rendered.yaml"; then
    echo 'upgrade rendered an empty container image' >&2
    exit 1
  fi
}

# Direct Helm --reuse-values compatibility: neither new key is present.
"$helm_tool" template oberth "$legacy_chart" --is-upgrade --namespace oberth \
  --set-string "image.ref=$server_image" >"$work/rendered.yaml"
check_default

# The reset-then-reuse path merges new defaults with the old release's values.
"$helm_tool" template oberth "$chart" --is-upgrade --namespace oberth \
  --values "$fixture" --set-string "image.ref=$server_image" >"$work/rendered.yaml"
check_default

# A previously stored mirror/backend choice (including a forward-repair value)
# wins over the CLI default, before any fresh --set overrides are supplied.
# Use the opt-in backend so retaining the default cannot conceal lost values.
cp -- "$fixture" "$work/stored-values.yaml"
printf '\nutilityImage: %s\ngitBackend: go-git\n' "$mirror" >>"$work/stored-values.yaml"
"$helm_tool" template oberth "$chart" --is-upgrade --namespace oberth \
  --values "$work/stored-values.yaml" --set-string "image.ref=$server_image" >"$work/rendered.yaml"
grep -Fq -- "image: \"$mirror\"" "$work/rendered.yaml"
grep -Fq -- '--git-backend=go-git' "$work/rendered.yaml"

# Operator mirrors and the one-release backend switch retain precedence.
for source in "$legacy_chart" "$chart"; do
  "$helm_tool" template oberth "$source" --is-upgrade --namespace oberth \
    --values "$fixture" --set-string "image.ref=$server_image" \
    --set-string "utilityImage=$mirror" --set-string gitBackend=go-git >"$work/rendered.yaml"
  grep -Fq -- "image: \"$mirror\"" "$work/rendered.yaml"
  grep -Fq -- '--git-backend=go-git' "$work/rendered.yaml"
done

# The shadow setting also works when a reused release has no gitcache map.
for source in "$legacy_chart" "$chart"; do
  for shadow in true false; do
    expected=0
    if [ "$shadow" = true ]; then expected=1; fi
    "$helm_tool" template oberth "$source" --is-upgrade --namespace oberth \
      --values "$fixture" --set-string "image.ref=$server_image" \
      --set "gitcache.shadow=$shadow" >"$work/rendered.yaml"
    grep -A1 -F 'name: OBERTH_GITCACHE_SHADOW' "$work/rendered.yaml" | grep -Fq "value: \"$expected\""
  done
done
if "$helm_tool" template oberth "$chart" --set-string "image.ref=$server_image" \
  --set-string gitcache.shadow=true >"$work/rendered.yaml" 2>/dev/null; then
  echo 'gitcache.shadow must reject a string boolean' >&2
  exit 1
fi
