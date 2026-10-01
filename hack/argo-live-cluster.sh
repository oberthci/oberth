#!/usr/bin/env bash
# Builds a disposable local cluster running a real Argo Workflows controller,
# for the live Argo engine suite in internal/argojob.
#
#   hack/argo-live-cluster.sh up      # create, install Argo, print the exports
#   hack/argo-live-cluster.sh down    # delete the cluster
#
# The kubeconfig is written to a dedicated file rather than ~/.kube/config so
# creating this cluster never switches the current context of a shell that is
# pointed at a real cluster.
set -euo pipefail

CLUSTER="${OBERTH_ARGO_CLUSTER:-oberth-argo-live}"
KUBECONFIG_PATH="${OBERTH_ARGO_LIVE_KUBECONFIG:-${TMPDIR:-/tmp}/oberth-argo-live.kubeconfig}"
ARGO_VERSION="${OBERTH_ARGO_VERSION:-v3.7.3}"
ARGO_MANIFEST="https://github.com/argoproj/argo-workflows/releases/download/${ARGO_VERSION}/namespace-install.yaml"
ARGO_MANIFEST_SHA256_DEFAULT="b27f850f522a753f8d30f5cf22db4e7b982e6cf8e62acdb8564b48d3cf2e5fc2"
ARGO_MANIFEST_SHA256="${OBERTH_ARGO_MANIFEST_SHA256:-$ARGO_MANIFEST_SHA256_DEFAULT}"

if [ "${OBERTH_ARGO_VERSION:-}" != "" ] && [ "${OBERTH_ARGO_MANIFEST_SHA256:-}" = "" ]; then
    echo "FATAL: OBERTH_ARGO_VERSION is overridden but OBERTH_ARGO_MANIFEST_SHA256 is not set." >&2
    echo "Provide the SHA256 of the namespace-install.yaml for ${ARGO_VERSION}." >&2
    exit 1
fi

usage() {
    echo "usage: $0 {up|down}" >&2
    exit 64
}

up() {
    kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG_PATH"

    # Download and SHA256-verify the Argo manifest before applying.
    manifest_file="$(mktemp)"
    trap 'rm -f "$manifest_file"' EXIT
    curl --fail --silent --show-error --location \
        --proto '=https' --tlsv1.2 \
        --connect-timeout 10 --max-time 120 \
        --max-filesize 10485760 \
        --output "$manifest_file" \
        "$ARGO_MANIFEST"
    manifest_sha="$(sha256sum "$manifest_file" | cut -d ' ' -f 1)"
    if [ "$manifest_sha" != "$ARGO_MANIFEST_SHA256" ]; then
        rm -f "$manifest_file"
        echo "FATAL: Argo manifest SHA256 mismatch: got $manifest_sha, want $ARGO_MANIFEST_SHA256" >&2
        exit 1
    fi

    # Server-side apply: the Argo CRDs' openAPIV3Schema exceeds the 256KiB
    # last-applied-configuration annotation that client-side apply writes.
    KUBECONFIG="$KUBECONFIG_PATH" kubectl apply --server-side --force-conflicts -f "$manifest_file"
    rm -f "$manifest_file"
    KUBECONFIG="$KUBECONFIG_PATH" kubectl rollout status deploy/workflow-controller --timeout=300s

    cat <<EOF

Live Argo cluster ready. Run the engine suite with:

  export OBERTH_ARGO_LIVE_KUBECONFIG=$KUBECONFIG_PATH
  go test ./internal/argojob/ -run Live -count=1 -v

To additionally run the repository's OWN .oberth/build.yaml end to end
(TestLiveRepositoryBuildPipelineExecutes):

  go test ./internal/argojob/ -run TestLiveRepositoryBuildPipelineExecutes \\
      -count=1 -v -timeout 80m

The source needs no preparation: the test drives the same SourceSeeder the
server uses, so it copies this checkout into a claim it creates in the pipeline
namespace, exactly as a real run does. An earlier version of this script seeded
that claim by hand, which is precisely why the suite never noticed that a
pipeline cannot mount the server's own claim across a namespace boundary.

That run takes 15-20 minutes: it compiles golangci-lint, trivy and helm from
source and runs the race suite, for real, in real Pods.

It also needs working egress from Pods to proxy.golang.org, github.com and
ghcr.io. On a network that hijacks those (a captive portal resolving them to a
gateway address), point the steps at local mirrors with
OBERTH_ARGO_LIVE_PIPELINE_FILE, which takes a variant of build.yaml. The
host's own module cache is already a valid Go module proxy:

  (cd \$(go env GOMODCACHE)/cache/download && python3 -m http.server 8081)

then set GOPROXY to it in the variant. The variant is a lab artifact; the
committed build.yaml always names the real upstreams.
EOF
}

down() {
    kind delete cluster --name "$CLUSTER"
    rm -f "$KUBECONFIG_PATH"
}

case "${1:-}" in
    up) up ;;
    down) down ;;
    *) usage ;;
esac
