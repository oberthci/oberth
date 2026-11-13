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

render >"$work/default.yaml"
test "$(grep -c '^kind: PriorityClass$' "$work/default.yaml")" -eq 1
grep -Fxq '  name: oberth-oberth-control-plane' "$work/default.yaml"
test "$(grep -c '^value: 1000000$' "$work/default.yaml")" -eq 1
test "$(grep -c '^globalDefault: false$' "$work/default.yaml")" -eq 1
test "$(grep -c '^preemptionPolicy: PreemptLowerPriority$' "$work/default.yaml")" -eq 1
test "$(grep -c '^      priorityClassName:' "$work/default.yaml")" -eq 1
grep -Fxq '      priorityClassName: oberth-oberth-control-plane' "$work/default.yaml"

# A stored values file without a scheduling toggle still protects recovery.
cp -R -- "$chart" "$work/reused"
chmod -R u+w "$work/reused"
cp -- "$chart/values.yaml" "$work/reused/values.yaml"
"$helm_tool" template oberth "$work/reused" --namespace oberth >"$work/reused.yaml"
test "$(grep -c '^kind: PriorityClass$' "$work/reused.yaml")" -eq 1
grep -Fxq '      priorityClassName: oberth-oberth-control-plane' "$work/reused.yaml"

"$helm_tool" template second "$chart" --namespace alternate --set argo.namespace=other-pipelines >"$work/alternate.yaml"
grep -Fxq '  name: alternate-second-oberth-control-plane' "$work/alternate.yaml"
if render --set platformProtection.enabled=false >"$work/invalid.yaml" 2>&1; then
  echo 'unknown scheduling toggle accepted' >&2
  exit 1
fi

# Exercise the real pinned Argo chart, not a stand-in controller template.
# These are the installer's namespace/identity options, without network access.
argo_render() {
  "$helm_tool" template argo-workflows \
    "$root/internal/installer/testdata/argo-workflows-1.0.24.tgz" \
    --namespace oberth-argo --set singleNamespace=true \
    --set server.enabled=false --set crds.install=false \
    --set createAggregateRoles=false \
    --set controller.clusterWorkflowTemplates.enabled=false \
    --set workflow.serviceAccount.create=false --set workflow.rbac.create=false
}
argo_render >"$work/argo-off.yaml"
if grep -Eq '^[[:space:]]+(podPriorityClassName|priorityClassName):' "$work/argo-off.yaml"; then
  echo 'baseline Argo chart unexpectedly assigns workload priority' >&2
  exit 1
fi
openbao_render() {
  "$helm_tool" template openbao \
    "$bao_chart" \
    --namespace openbao --show-only templates/server-statefulset.yaml
}
openbao_render >"$work/bao-off.yaml"
if grep -q 'priorityClassName:' "$work/bao-off.yaml"; then
  echo 'baseline OpenBao chart unexpectedly assigns platform priority' >&2
  exit 1
fi
grep -A1 -Fx '  updateStrategy:' "$work/bao-off.yaml" | grep -Fxq '    type: OnDelete'
if grep -Eq '^[[:space:]]+(cpu: 100m|memory: 128Mi)$' "$work/argo-off.yaml" "$work/bao-off.yaml"; then
  echo 'dependency chart gained the retired managed requests' >&2
  exit 1
fi

echo 'platform protection chart contracts passed'
