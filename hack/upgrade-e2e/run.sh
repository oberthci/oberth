#!/usr/bin/env bash
# Rehearse the published Oberth release -> exact local candidates in Docker.
set -euo pipefail
umask 077
here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo=${E2E_REPO:-}
if [[ -z $repo ]]; then
  repo=$(git -C "$here" rev-parse --show-toplevel)
fi
repo=$(git -C "$repo" rev-parse --show-toplevel)
evidence=${E2E_EVIDENCE:-$(mktemp -d /tmp/oberth-upgrade-evidence.XXXXXXXX)}
mkdir -p "$evidence"
evidence=$(cd "$evidence" && pwd)
# An optional executable wrapper receives the command and its arguments.
if [[ -n ${E2E_WRAPPER:-} ]]; then
  exec env -u E2E_WRAPPER "$E2E_WRAPPER" env E2E_REPO="$repo" E2E_EVIDENCE="$evidence" bash "$here/run.sh"
fi
helm=${E2E_HELM:-helm}
for tool in k3d kubectl "$helm" docker go jq cosign curl sha256sum git rg; do command -v "$tool" >/dev/null; done
private=$(mktemp -d /tmp/oberth-upgrade-e2e.XXXXXXXX)
cluster="ob-upgrade-$$"
created=false
fixture_image="oberth-upgrade-e2e:fixture-$$"
images=()
declare -A candidate_refs=()
export KUBECONFIG="$private/kubeconfig" DOCKER_CONFIG="$private/docker"
export HELM_CONFIG_HOME="$private/helm-config" HELM_CACHE_HOME="$private/helm-cache" HELM_DATA_HOME="$private/helm-data"
mkdir -p "$DOCKER_CONFIG"
k() { kubectl --context="k3d-$cluster" "$@"; }
cleanup() {
  local result=$?
  trap - EXIT INT TERM
  if $created; then
    # Avoid secret resources and identity-store logs, even on failure.
    k get pods -A -o wide >"$evidence/final-pods.txt" 2>&1 || true
    k -n oberth logs deploy/oberth -c oberth >"$evidence/final-server.log" 2>&1 || true
    k get events -A --field-selector type=Warning >"$evidence/warnings.txt" 2>&1 || true
    if ! k3d cluster delete "$cluster" >"$evidence/teardown.log" 2>&1; then result=1; fi
    if k3d cluster list -o json | jq -e --arg n "$cluster" 'any(.[]; .name==$n)' >/dev/null; then result=1; fi
  fi
  for img in "${images[@]}"; do docker image rm "$img" >/dev/null 2>&1 || true; done
  find "$private" -delete
  printf 'exit=%s\n' "$result" >"$evidence/result.txt"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
fetch() { curl --fail --silent --show-error --location --proto '=https' --max-time 90 "$1" -o "$2"; }
release=https://releases.oberth.ci/oberth/v0.17.81
for file in cosign.pub release.json release.json.sigstore.json; do fetch "$release/$file" "$evidence/$file"; done
printf '%s  %s\n' 036dd71c9d3c07a19bff06d4392874014573f695620ac8554d9df175094c4a31 "$evidence/cosign.pub" | sha256sum -c -
cosign verify-blob --key "$evidence/cosign.pub" --insecure-ignore-tlog \
  --bundle "$evidence/release.json.sigstore.json" "$evidence/release.json"
jq -e '.component=="oberth" and .version=="v0.17.81" and .chart.version=="0.17.81"' "$evidence/release.json" >/dev/null
base=$(jq -er '.source.sha' "$evidence/release.json")
published=$(jq -er '.images.server.ref' "$evidence/release.json")
[[ $base =~ ^[0-9a-f]{40}$ && $published =~ ^europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:[0-9a-f]{64}$ ]]
fetch https://charts.oberth.ci/oberth-0.17.81.tgz "$evidence/oberth-0.17.81.tgz"
chart_hash=$(jq -er '.chart.digest | sub("^sha256:"; "")' "$evidence/release.json")
printf '%s  %s\n' "$chart_hash" "$evidence/oberth-0.17.81.tgz" | sha256sum -c -
"$helm" show values "$evidence/oberth-0.17.81.tgz" >"$private/published-values.yaml"
# A signed record must bind the image actually used by the published chart.
rg -F "ref: $published" "$private/published-values.yaml" >/dev/null
sha82=$(git -C "$repo" rev-parse 'cba75ca7^{commit}')
sha83=$(git -C "$repo" rev-parse --verify "${CANDIDATE_83:-refs/heads/fix/ob-83-rc}^{commit}" 2>/dev/null || true)
printf 'origin/main=%s\npublished=%s\ncandidate82=%s\ncandidate83=%s\n' \
  "$(git -C "$repo" rev-parse refs/remotes/origin/main)" "$base" "$sha82" "${sha83:-absent}" >"$evidence/sources.txt"
source_tree() {
  mkdir -p "$private/source-$1"
  git -C "$repo" archive "$2" | tar -x -C "$private/source-$1"
  cp "$here/admission_test.go.txt" "$private/source-$1/internal/service/upgrade_e2e_test.go"
}
source_tree 81 "$base"
source_tree 82 "$sha82"
if [[ -n $sha83 ]]; then source_tree 83 "$sha83"; fi
# Red must be a behavioral assertion, never a compilation/tooling failure.
if (cd "$private/source-81" && go test -p=2 -parallel=2 -count=1 ./internal/service -run '^TestUpgradeE2EMeasuredBackfillBehindHighest$' -timeout 2m) >"$evidence/backfill-red-81.log" 2>&1; then
  echo 'v0.17.81 unexpectedly accepted the regression test' >&2; exit 1
fi
rg -F 'measured short run must backfill without delaying the highest-priority head' "$evidence/backfill-red-81.log" >/dev/null
for version in 82 ${sha83:+83}; do
  (cd "$private/source-$version" && go test -p=2 -parallel=2 -count=1 ./internal/service -run '^TestUpgradeE2EMeasuredBackfillBehindHighest$' -timeout 2m) >"$evidence/backfill-green-$version.log" 2>&1
  (cd "$private/source-$version" && go test -p=2 -parallel=2 -count=1 ./internal/store -run '^(TestMigrationDefinitionsRejectDuplicateVersions|TestExistingV24MigrationLedgerPreservedWithoutReplay|TestQueuePriorityClassificationAndFIFO)$' -timeout 2m) >"$evidence/store-$version.log" 2>&1
  # Only the server binary changes. Refuse reuse if any runtime substrate
  # input differs; this is a local rehearsal image, not a release build.
  candidate=$sha82; [[ $version != 83 ]] || candidate=$sha83
  git -C "$repo" diff --exit-code "$base" "$candidate" -- Dockerfile internal/staticgit hack/build-static-git.sh hack/static-git-no-shell.patch hack/install-static-git-packages.sh >"$evidence/substrate-$version.diff"
  (cd "$private/source-$version" && go build -p=2 -trimpath -buildvcs=false -ldflags "-s -w -X main.version=v0.17.$version-e2e-${candidate:0:8}" -o "$private/oberth-$version" ./cmd/oberth)
done
mkdir -p "$private/source-81/hack/upgrade-e2e/fixture"
cp "$here/fixture/main.go" "$private/source-81/hack/upgrade-e2e/fixture/"
(cd "$private/source-81" && go build -p=2 -trimpath -buildvcs=false -o "$private/fixture" ./hack/upgrade-e2e/fixture)
docker pull "$published"
printf 'FROM %s\nCOPY --chmod=0755 fixture /fixture\nENTRYPOINT ["/fixture"]\n' "$published" >"$private/Dockerfile.fixture"
printf '*\n!fixture\n!oberth-82\n!oberth-83\n' >"$private/.dockerignore"
images+=("$fixture_image")
docker build -f "$private/Dockerfile.fixture" -t "$fixture_image" "$private"
for version in 82 ${sha83:+83}; do
  img="oberth-upgrade-e2e:$version-$$"; images+=("$img")
  printf 'FROM %s\nCOPY --chmod=0755 oberth-%s /usr/local/bin/oberth\n' "$published" "$version" >"$private/Dockerfile.$version"
  docker build -f "$private/Dockerfile.$version" -t "$img" "$private"
  docker image inspect "$img" --format '{{.Id}}' >"$evidence/image-$version.txt"
done
# Mark creation before starting: k3d may fail after allocating Docker objects.
created=true
k3d cluster create "$cluster" --image rancher/k3s:v1.34.3-k3s1 --servers 1 --agents 0 --no-lb \
  --kubeconfig-update-default=false --kubeconfig-switch-context=false \
  --k3s-arg '--disable=traefik@server:0' --k3s-arg '--disable=servicelb@server:0' --wait --timeout 180s
k3d kubeconfig get "$cluster" >"$KUBECONFIG"
k3d image import -c "$cluster" "${images[@]}"
# Helm requires immutable server references. Tag containerd's actual imported
# manifest digest locally; no registry upload or schema exception is needed.
for version in 82 ${sha83:+83}; do
  local_tag="docker.io/library/oberth-upgrade-e2e:$version-$$"
  digest=$(docker exec "k3d-$cluster-server-0" ctr images ls | awk -v ref="$local_tag" '$1==ref {print $3}')
  [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]]
  candidate_refs[$version]="docker.io/library/oberth-upgrade-e2e@$digest"
  docker exec "k3d-$cluster-server-0" ctr images tag "$local_tag" "${candidate_refs[$version]}"
  printf '%s\n' "${candidate_refs[$version]}" >"$evidence/image-ref-$version.txt"
done
k apply -f "$here/dependencies.yaml"
k -n oberth wait --for=condition=Ready pod/identity-store --timeout=180s
bao() { k -n oberth exec -i identity-store -- env HOME=/tmp BAO_ADDR=http://127.0.0.1:8200 bao "$@"; }
# The real CLI uses the dev server's own token helper entirely inside its pod.
# No caller token, upstream credential, or Kubernetes Secret is supplied.
bao secrets enable -path=oberth -version=2 kv
bao auth enable kubernetes
bao write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc:443
printf '%s\n' 'path "oberth/data/identities/oberth/*" { capabilities = ["create", "read", "update"] }' \
  'path "auth/token/revoke-self" { capabilities = ["update"] }' | bao policy write oberth-e2e -
bao write auth/kubernetes/role/oberth-ci bound_service_account_names=oberth bound_service_account_namespaces=oberth \
  token_policies=oberth-e2e token_no_default_policy=true token_ttl=10m token_max_ttl=15m
"$helm" install argo-workflows argo-workflows --repo https://argoproj.github.io/argo-helm --version 1.0.24 -n oberth-argo \
  --set singleNamespace=true --set server.enabled=false --set createAggregateRoles=false \
  --set controller.clusterWorkflowTemplates.enabled=false --set workflow.serviceAccount.create=false --set workflow.rbac.create=false \
  --set crds.install=true --set crds.keep=true --set crds.full=false \
  --set 'controller.extraEnv[0].name=POD_NAMES' --set-string 'controller.extraEnv[0].value=v1' --wait --timeout 180s
# A fresh server intentionally fails readiness until a local upstream exists.
"$helm" install oberth "$evidence/oberth-0.17.81.tgz" -n oberth --values "$here/values.yaml"
booted=false
for ((i=0;i<60;i++)); do
  if k -n oberth logs deploy/oberth -c oberth 2>/dev/null | rg -q 'ready for bootstrap'; then booted=true; break; fi
  sleep 2
done
$booted
k -n oberth scale deploy/oberth --replicas=0
k -n oberth wait --for=delete pod -l app.kubernetes.io/name=oberth --timeout=100s
fixture_pod() {
  local name=$1 mode=$2
  cat <<YAML | k create -f -
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: oberth
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsUser: 65534
    runAsGroup: 65534
    fsGroup: 65534
  containers:
    - name: fixture
      image: $fixture_image
      imagePullPolicy: Never
      args: [$mode, /data/oberth.sqlite]
      volumeMounts:
        - name: data
          mountPath: /data
        - name: tmp
          mountPath: /tmp
  volumes:
    - name: tmp
      emptyDir:
        sizeLimit: 128Mi
    - name: data
      persistentVolumeClaim:
        claimName: oberth-data
YAML
  if ! k -n oberth wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$name" --timeout=60s; then
    k -n oberth logs "$name" >"$evidence/$name-error.log" 2>&1 || true
    return 1
  fi
}
fixture_pod fixture-seed seed
k -n oberth logs fixture-seed >"$evidence/workflow.json"
k create -f "$evidence/workflow.json"
k -n oberth-argo wait --for=jsonpath='{.status.phase}'=Running workflow/oberth-e2e-running --timeout=60s
k -n oberth delete pod fixture-seed --wait=true
k -n oberth scale deploy/oberth --replicas=1
k -n oberth rollout status deploy/oberth --timeout=150s
snapshot() {
  local phase=$1
  fixture_pod "fixture-$phase" snapshot
  k -n oberth logs "fixture-$phase" >"$evidence/state-$phase.json"
  jq -e '.Total==5 and (.Runs|length)==5 and ([.Runs[]|select(.Status=="running")]|length)==1 and ([.Runs[]|select(.Status=="queued")]|length)==4 and ([.Runs[].Priority]|unique|sort)==["high","highest","normal"]' "$evidence/state-$phase.json" >/dev/null
  k -n oberth delete pod "fixture-$phase" --wait=true
  k get priorityclasses -o json | jq -S '[.items[] | select(.metadata.labels."app.kubernetes.io/instance"=="oberth") | {name:.metadata.name,uid:.metadata.uid,value,globalDefault,preemptionPolicy}] | sort_by(.name)' >"$evidence/priorities-$phase.json"
  jq -e 'length>0' "$evidence/priorities-$phase.json" >/dev/null
  k -n oberth get pods -l app.kubernetes.io/name=oberth -o json >"$evidence/pods-$phase.json"
  jq -e '.items|length==1 and all(.[]; .status.phase=="Running" and all(.status.containerStatuses[]; .ready==true and .restartCount==0))' "$evidence/pods-$phase.json" >/dev/null
  local expected_image=$published
  [[ $phase == 81 ]] || expected_image=${candidate_refs[${phase%%-*}]}
  jq -e --arg img "$expected_image" '.items[0].spec.containers[] | select(.name=="oberth") | .image==$img' "$evidence/pods-$phase.json" >/dev/null
  k -n oberth exec deploy/oberth -c oberth -- /usr/local/bin/oberth version >"$evidence/version-$phase.txt"
  rg -F "v0.17.${phase%%-*}" "$evidence/version-$phase.txt" >/dev/null
  k -n oberth-argo get workflow oberth-e2e-running -o json | jq '{uid:.metadata.uid,phase:.status.phase}' >"$evidence/workflow-$phase.json"
  jq -e '.phase=="Running"' "$evidence/workflow-$phase.json" >/dev/null
  k -n oberth get deploy oberth -o jsonpath='{.spec.template.spec.priorityClassName}' >"$evidence/pod-priority-$phase.txt"
}
snapshot 81
for version in 82 ${sha83:+83}; do
  "$helm" upgrade oberth "$private/source-$version/charts/oberth" -n oberth --reuse-values \
    --set-string "image.ref=${candidate_refs[$version]}" --set image.pullPolicy=Never --wait --timeout 180s
  snapshot "$version"
  for artifact in state priorities workflow pod-priority; do
    ext=json; [[ $artifact != pod-priority ]] || ext=txt
    cmp "$evidence/$artifact-81.$ext" "$evidence/$artifact-$version.$ext"
  done
  # Restart the same target once more to detect migration replays or drift.
  k -n oberth rollout restart deploy/oberth
  k -n oberth rollout status deploy/oberth --timeout=180s
  snapshot "$version-restart"
  for artifact in state priorities workflow pod-priority; do
    ext=json; [[ $artifact != pod-priority ]] || ext=txt
    cmp "$evidence/$artifact-81.$ext" "$evidence/$artifact-$version-restart.$ext"
  done
done
k -n oberth get secrets -o json | jq -e '[.items[]|select(.type!="helm.sh/release.v1")]|length==0' >/dev/null
printf 'PASS: release signature, behavioral red/green, five durable runs, ledger timestamps, priority classes, Workflow UID, upgrades and repeat restarts\n'
