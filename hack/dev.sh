#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
source hack/steps.sh
main_path=${DEV_MAIN_PATH:-./hack/devloop/placeholder}
chart_path=${DEV_CHART_PATH:-hack/devloop/placeholder/chart}
config=$HOME/.config/oberth-vm/next-dev.kubeconfig
docker=(docker --context colima-oberth-next)
started=$SECONDS
image_file=$(mktemp "$GOTMPDIR/oberth-dev-image.XXXXXX")
trap 'rm -f "$image_file"' EXIT
run "${docker[@]}" build --build-arg "MAIN_PATH=$main_path" \
    --iidfile "$image_file" -f hack/devloop/Dockerfile .
built=$SECONDS
tag=$(cut -d: -f2 "$image_file")
run "${docker[@]}" tag "$(cat "$image_file")" "localhost:5001/oberth-dev:$tag"
run "${docker[@]}" push "localhost:5001/oberth-dev:$tag"
pushed=$SECONDS
run helm --kubeconfig "$config" --kube-context k3d-next-dev upgrade --install oberth-dev "$chart_path" \
    --namespace oberth-dev --create-namespace --set-string "image=k3d-rw-registry:5000/oberth-dev:$tag" \
    --wait --timeout 120s --history-max 2
printf 'dev: build=%ss push=%ss ready=%ss total=%ss\n' \
    "$((built-started))" "$((pushed-built))" "$((SECONDS-pushed))" "$((SECONDS-started))"
