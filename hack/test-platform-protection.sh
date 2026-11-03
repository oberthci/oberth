#!/bin/sh
set -eu

chart=${1:-charts/oberth}
helm_tool=${HELM:-helm}
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
root=$(dirname -- "$script_dir")
bao_chart=$root/internal/installer/testdata/openbao-0.28.6.tgz
if command -v sha256sum >/dev/null 2>&1; then
  bao_digest=$(sha256sum "$bao_chart" | awk '{print $1}')
else
  bao_digest=$(shasum -a 256 "$bao_chart" | awk '{print $1}')
fi
if test "$bao_digest" != 175c5cea2d36b68d348eca872044656bd8740c4dbe26b7dc8eb7c7438474a8b3; then
  echo 'OpenBao chart fixture SHA256 mismatch' >&2
  exit 1
fi
work=$(mktemp -d)
trap 'find "$work" -delete' EXIT

render() {
  "$helm_tool" template oberth "$chart" --namespace oberth "$@"
}

render --set watchTunnel.enabled=true >"$work/default.yaml"
render --set watchTunnel.enabled=true --set platformProtection.enabled=false >"$work/off.yaml"
if grep -Eq 'kind: PriorityClass|priorityClassName:' "$work/off.yaml"; then
  echo 'explicitly disabled platform priority still rendered protection' >&2
  exit 1
fi

# Simulate --reuse-values with a stored values shape predating this feature.
cp -R -- "$chart" "$work/legacy"
chmod -R u+w "$work/legacy"
cp -- "$script_dir/testdata/chart-values-v0.17.53.yaml" "$work/legacy/values.yaml"
"$helm_tool" template oberth "$work/legacy" --namespace oberth >"$work/legacy.yaml"
if grep -Eq 'kind: PriorityClass|priorityClassName:' "$work/legacy.yaml"; then
  echo 'absent platformProtection enabled platform priority' >&2
  exit 1
fi

render --set platformProtection.enabled=true >"$work/no-tunnel.yaml"
test "$(grep -c '^kind: PriorityClass$' "$work/no-tunnel.yaml")" -eq 4
test "$(grep -c '^      priorityClassName:' "$work/no-tunnel.yaml")" -eq 1
render --set watchTunnel.enabled=true --set platformProtection.enabled=true >"$work/on.yaml"
# Stage C protects platform workloads by default. Explicit opt-out and raw
# historical values remain covered separately below and above.
diff -u "$work/default.yaml" "$work/on.yaml"
test "$(grep -c '^kind: PriorityClass$' "$work/on.yaml")" -eq 4
test "$(grep -c '^value: 1000000$' "$work/on.yaml")" -eq 4
test "$(grep -c '^globalDefault: false$' "$work/on.yaml")" -eq 4
test "$(grep -c '^preemptionPolicy: PreemptLowerPriority$' "$work/on.yaml")" -eq 4
test "$(awk '/^kind:/ { priority=($0 == "kind: PriorityClass") } priority && /^    helm.sh\/resource-policy: keep$/ { count++ } END { print count+0 }' "$work/on.yaml")" -eq 2
for component in argo-controller openbao; do
  grep -A2 -Fx "  name: oberth-oberth-platform-$component" "$work/on.yaml" | \
    grep -Fxq '    helm.sh/resource-policy: keep'
done
test "$(grep -c '^      priorityClassName:' "$work/on.yaml")" -eq 2
for component in oberth cloudflared argo-controller openbao; do
  grep -Fxq "  name: oberth-oberth-platform-$component" "$work/on.yaml"
done
grep -Fxq '      priorityClassName: oberth-oberth-platform-oberth' "$work/on.yaml"
grep -Fxq '      priorityClassName: oberth-oberth-platform-cloudflared' "$work/on.yaml"

# Every other rendered byte, including CI configuration, requests, limits and
# admission capacity, must stay identical. No class on workflow defaults/helpers.
awk '
  function emit() { if (doc !~ /kind: PriorityClass/) printf "%s", doc }
  /^---$/ { emit(); doc=$0 ORS; next }
  /^      priorityClassName:/ { next }
  { doc=doc $0 ORS }
  END { emit() }
' "$work/on.yaml" >"$work/ordinary.yaml"
diff -u "$work/off.yaml" "$work/ordinary.yaml"

# Cluster-scoped names must differ for the same release in another namespace.
"$helm_tool" template oberth "$chart" --namespace alternate \
  --set platformProtection.enabled=true >"$work/alternate.yaml"
grep -Fxq '  name: alternate-oberth-platform-argo-controller' "$work/alternate.yaml"
"$helm_tool" template second "$chart" --namespace oberth \
  --set platformProtection.enabled=true >"$work/second.yaml"
grep -Fxq '  name: oberth-second-oberth-platform-argo-controller' "$work/second.yaml"

# Reject misspelled knobs and invalid booleans rather than silently enabling.
for setting in platformProtection.enable=true platformProtection.enabled=invalid; do
  if render --set "$setting" >"$work/invalid.yaml" 2>&1; then
    echo "invalid platform protection setting accepted: $setting" >&2
    exit 1
  fi
done

# Exercise the real pinned Argo chart, not a stand-in controller template.
# These are the installer's namespace/identity options, without network access.
argo_render() {
  "$helm_tool" template argo-workflows \
    "$root/internal/installer/testdata/argo-workflows-1.0.24.tgz" \
    --namespace oberth-argo --set singleNamespace=true \
    --set server.enabled=false --set crds.install=false \
    --set createAggregateRoles=false \
    --set controller.clusterWorkflowTemplates.enabled=false \
    --set workflow.serviceAccount.create=false --set workflow.rbac.create=false "$@"
}
argo_render >"$work/argo-off.yaml"
if grep -Eq '^[[:space:]]+(podPriorityClassName|priorityClassName):' "$work/argo-off.yaml"; then
  echo 'baseline Argo chart unexpectedly assigns workload priority' >&2
  exit 1
fi
argo_render -f "$root/deploy/platform-protection/argo-workflows-values.yaml" >"$work/argo-on.yaml"
test "$(grep -c '^[[:space:]]*priorityClassName:' "$work/argo-on.yaml")" -eq 1
grep -Fxq '      priorityClassName: oberth-oberth-platform-argo-controller' "$work/argo-on.yaml"
grep -Eq '^[[:space:]]+cpu: 25m$' "$work/argo-on.yaml"
grep -Eq '^[[:space:]]+memory: 128Mi$' "$work/argo-on.yaml"
if grep -Eq '^[[:space:]]+(podPriorityClassName|limits):' "$work/argo-on.yaml"; then
  echo 'Argo overlay added workflow priority or resource limits' >&2
  exit 1
fi

openbao_render() {
  "$helm_tool" template openbao \
    "$bao_chart" \
    --namespace openbao --show-only templates/server-statefulset.yaml "$@"
}
openbao_render >"$work/bao-off.yaml"
if grep -q 'priorityClassName:' "$work/bao-off.yaml"; then
  echo 'baseline OpenBao chart unexpectedly assigns platform priority' >&2
  exit 1
fi
openbao_render -f "$root/deploy/platform-protection/openbao-values.yaml" >"$work/bao-on.yaml"
for source in "$work/bao-off.yaml" "$work/bao-on.yaml"; do
  grep -A1 -Fx '  updateStrategy:' "$source" | grep -Fxq '    type: OnDelete'
done
test "$(grep -c '^[[:space:]]*priorityClassName:' "$work/bao-on.yaml")" -eq 1
grep -Fxq '      priorityClassName: oberth-oberth-platform-openbao' "$work/bao-on.yaml"
grep -Eq '^[[:space:]]+cpu: 100m$' "$work/bao-on.yaml"
grep -Eq '^[[:space:]]+memory: 64Mi$' "$work/bao-on.yaml"
if grep -Eq '^[[:space:]]+limits:' "$work/bao-on.yaml"; then
  echo 'OpenBao overlay introduced resource limits' >&2
  exit 1
fi

echo 'platform protection chart contracts passed'
