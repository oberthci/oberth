#!/usr/bin/env bash
# Rehearse the published Oberth release -> exact local candidates in Docker.
set -euo pipefail
umask 077
from=${FROM_VERSION:-}
read -r -a candidates <<< "${CANDIDATES:-}"
while (($#)); do
  case $1 in
    --from) from=${2:?--from requires a version}; shift 2 ;;
    --candidate) candidates+=("${2:?--candidate requires a ref}"); shift 2 ;;
    --help) echo 'Usage: run.sh --from VERSION --candidate REF [--candidate REF ...]'; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done
from=${from#v}
[[ $from =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && ${#candidates[@]} -gt 0 ]] || {
  echo 'Supply --from VERSION and at least one --candidate REF' >&2; exit 2;
}
export FROM_VERSION=$from CANDIDATES="${candidates[*]}"
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
release=https://releases.oberth.ci/oberth/v$from
for file in cosign.pub release.json release.json.sigstore.json; do fetch "$release/$file" "$evidence/$file"; done
printf '%s  %s\n' 036dd71c9d3c07a19bff06d4392874014573f695620ac8554d9df175094c4a31 "$evidence/cosign.pub" | sha256sum -c -
cosign verify-blob --key "$evidence/cosign.pub" --insecure-ignore-tlog \
  --bundle "$evidence/release.json.sigstore.json" "$evidence/release.json"
jq -e --arg v "$from" '.component=="oberth" and .version==("v"+$v) and .chart.version==$v' "$evidence/release.json" >/dev/null
base=$(jq -er '.source.sha' "$evidence/release.json")
published=$(jq -er '.images.server.ref' "$evidence/release.json")
[[ $base =~ ^[0-9a-f]{40}$ && $published =~ ^[a-zA-Z0-9./:_-]+@sha256:[0-9a-f]{64}$ ]]
chart="$evidence/oberth-$from.tgz"
fetch "https://charts.oberth.ci/oberth-$from.tgz" "$chart"
chart_hash=$(jq -er '.chart.digest | sub("^sha256:"; "")' "$evidence/release.json")
printf '%s  %s\n' "$chart_hash" "$chart" | sha256sum -c -
"$helm" show values "$chart" >"$private/published-values.yaml"
# A signed record must bind the image actually used by the published chart.
rg -F "ref: $published" "$private/published-values.yaml" >/dev/null
declare -A candidate_shas=() candidate_versions=()
versions=()
printf 'published=%s\nfrom=%s\n' "$base" "$from" >"$evidence/sources.txt"
for i in "${!candidates[@]}"; do
  version="candidate-$((i+1))"
  versions+=("$version")
  candidate_shas[$version]=$(git -C "$repo" rev-parse --verify "${candidates[$i]}^{commit}")
  printf '%s=%s\n' "$version" "${candidate_shas[$version]}" >>"$evidence/sources.txt"
done
source_tree() {
  mkdir -p "$private/source-$1"
  git -C "$repo" archive "$2" | tar -x -C "$private/source-$1"
  cp "$here/admission_test.go.txt" "$private/source-$1/internal/service/upgrade_e2e_test.go"
}
source_tree baseline "$base"
for version in "${versions[@]}"; do source_tree "$version" "${candidate_shas[$version]}"; done
# Red must be a behavioral assertion, never a compilation/tooling failure.
if (cd "$private/source-baseline" && go test -p=2 -parallel=2 -count=1 ./internal/service -run '^TestUpgradeE2EMeasuredBackfillBehindHighest$' -timeout 2m) >"$evidence/backfill-red-baseline.log" 2>&1; then
  echo "v$from unexpectedly accepted the regression test" >&2; exit 1
fi
rg -F 'measured short run must backfill without delaying the highest-priority head' "$evidence/backfill-red-baseline.log" >/dev/null
for version in "${versions[@]}"; do
  (cd "$private/source-$version" && go test -p=2 -parallel=2 -count=1 ./internal/service -run '^TestUpgradeE2EMeasuredBackfillBehindHighest$' -timeout 2m) >"$evidence/backfill-green-$version.log" 2>&1
  (cd "$private/source-$version" && go test -p=2 -parallel=2 -count=1 ./internal/store -run '^(TestMigrationDefinitionsRejectDuplicateVersions|TestExistingV24MigrationLedgerPreservedWithoutReplay|TestQueuePriorityClassificationAndFIFO)$' -timeout 2m) >"$evidence/store-$version.log" 2>&1
  # Only the server binary changes. Refuse reuse if any runtime substrate
  # input differs; this is a local rehearsal image, not a release build.
  candidate=${candidate_shas[$version]}
  candidate_versions[$version]=$("$helm" show chart "$private/source-$version/charts/oberth" | awk '$1=="appVersion:" {gsub(/"/, "", $2); print $2}')
  [[ ${candidate_versions[$version]} =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
  git -C "$repo" diff --exit-code "$base" "$candidate" -- Dockerfile internal/staticgit hack/build-static-git.sh hack/static-git-no-shell.patch hack/install-static-git-packages.sh >"$evidence/substrate-$version.diff"
  (cd "$private/source-$version" && go build -p=2 -trimpath -buildvcs=false -ldflags "-s -w -X main.version=${candidate_versions[$version]}-e2e-${candidate:0:8}" -o "$private/oberth-$version" ./cmd/oberth)
done
for version in baseline "${versions[@]}"; do
  mkdir -p "$private/source-$version/hack/upgrade-e2e/fixture"
  cp "$here"/fixture/*.go "$private/source-$version/hack/upgrade-e2e/fixture/"
  (cd "$private/source-$version" && go test -p=2 -parallel=2 ./hack/upgrade-e2e/fixture && go build -p=2 -trimpath -buildvcs=false -o "$private/fixture-$version" ./hack/upgrade-e2e/fixture)
done
last=${versions[${#versions[@]}-1]}
bash "$here/migration-probe.sh" "$private/source-$last" "$private/fixture-baseline" \
  "$("$private/oberth-$last" version --schema)" "$published" "$evidence"
docker pull "$published"
printf 'FROM %s\nCOPY --chmod=0755 fixture-baseline /fixture\nENTRYPOINT ["/fixture"]\n' "$published" >"$private/Dockerfile.fixture"
printf '*\n!fixture-*\n!oberth-candidate-*\n' >"$private/.dockerignore"
images+=("$fixture_image")
docker build -f "$private/Dockerfile.fixture" -t "$fixture_image" "$private"
for version in "${versions[@]}"; do
  img="oberth-upgrade-e2e:$version-$$"; images+=("$img")
  printf 'FROM %s\nCOPY --chmod=0755 oberth-%s /usr/local/bin/oberth\nCOPY --chmod=0755 fixture-%s /fixture\n' "$published" "$version" "$version" >"$private/Dockerfile.$version"
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
for version in "${versions[@]}"; do
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
  --set controller.workflowDefaults.spec.suspend=true \
  --set 'controller.extraEnv[0].name=POD_NAMES' --set-string 'controller.extraEnv[0].value=v1' --wait --timeout 180s
# A fresh server intentionally fails readiness until a local upstream exists.
"$helm" install oberth "$chart" -n oberth --values "$here/values.yaml" \
  --set-string "runnerImagePrefixes[0]=${published%@sha256:*}"
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
  local extra=
  [[ $mode != seed ]] || extra=", $published"
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
      command: [/fixture]
      args: [$mode, /data/oberth.sqlite$extra]
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
    # A suspended host-check process can resume after the watch deadline even
    # though its Pod completed. Recheck the same required terminal condition.
    if [[ $(k -n oberth get pod "$name" -o jsonpath='{.status.phase}') != Succeeded ]]; then
      k -n oberth logs "$name" >"$evidence/$name-error.log" 2>&1 || true
      return 1
    fi
    printf 'Confirmed pod/%s Succeeded after interrupted wait\n' "$name"
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
  local phase=$1 key=${2:-${1%-restart}}
  fixture_pod "fixture-$phase" snapshot
  k -n oberth logs "fixture-$phase" >"$evidence/state-$phase.json"
  jq -e '.Total>=5 and (.Runs|length)==5 and ([.Runs[]|select(.Status=="running")]|length)==1 and ([.Runs[]|select(.Status=="queued")]|length)==4 and ([.Runs[].Priority]|unique|sort)==["high","highest","normal"]' "$evidence/state-$phase.json" >/dev/null
  k -n oberth delete pod "fixture-$phase" --wait=true
  k get priorityclasses -o json | jq -S '[.items[] | select(.metadata.labels."app.kubernetes.io/instance"=="oberth") | {name:.metadata.name,uid:.metadata.uid,value,globalDefault,preemptionPolicy}] | sort_by(.name)' >"$evidence/priorities-$phase.json"
  jq -e 'length>0' "$evidence/priorities-$phase.json" >/dev/null
  k -n oberth get pods -l app.kubernetes.io/name=oberth -o json >"$evidence/pods-$phase.json"
  jq -e '.items|length==1 and all(.[]; .status.phase=="Running" and all(.status.containerStatuses[]; .ready==true and .restartCount==0))' "$evidence/pods-$phase.json" >/dev/null
  local expected_image=$published
  [[ $key == baseline ]] || expected_image=${candidate_refs[$key]}
  jq -e --arg img "$expected_image" '.items[0].spec.containers[] | select(.name=="oberth") | .image==$img' "$evidence/pods-$phase.json" >/dev/null
  k -n oberth exec deploy/oberth -c oberth -- /usr/local/bin/oberth version >"$evidence/version-$phase.txt"
  local expected_version=v$from
  [[ $key == baseline ]] || expected_version=${candidate_versions[$key]}
  rg -F "$expected_version" "$evidence/version-$phase.txt" >/dev/null
  k -n oberth-argo get workflow "$running_workflow" -o json | jq '{uid:.metadata.uid,phase:.status.phase}' >"$evidence/workflow-$phase.json"
  jq -e '.phase=="Running"' "$evidence/workflow-$phase.json" >/dev/null
  k -n oberth get deploy oberth -o jsonpath='{.spec.template.spec.priorityClassName}' >"$evidence/pod-priority-$phase.txt"
}
running_workflow=oberth-e2e-running
# Release the holder and observe real daemon admission, in priority order. The
# controller suspends generated Workflows, keeping build containers idle. Keep
# the second admitted Workflow as the next holder, then replenish two waiters.
# Each subsequent upgrade compares with this explicit, newly frozen checkpoint.
schedule_probe() {
  local phase=$1 key=$2 expected id job
  for expected in highest high; do
    id=$(jq -er --arg p "$expected" '.Runs[]|select(.Status=="queued" and .Priority==$p)|.ID' "$evidence/state-$phase.json")
    k -n oberth-argo delete workflow "$running_workflow" --wait=true
    job=
    for ((attempt=0;attempt<90;attempt++)); do
      job=$(k -n oberth-argo get workflows -l "oberth.ci/run=$id" -o json | jq -r '.items[0].metadata.name // empty')
      [[ -z $job ]] || break
      sleep 2
    done
    if [[ -z $job ]]; then
      fixture_pod "diagnose-$phase" diagnose
      k -n oberth logs "diagnose-$phase" >"$evidence/queue-error-$phase.jsonl"
      echo "scheduler did not admit $expected run $id" >&2; return 1
    fi
    k -n oberth-argo wait --for=jsonpath='{.status.phase}'=Running "workflow/$job" --timeout=60s
    k -n oberth-argo get workflow "$job" -o json | jq '{name:.metadata.name,uid:.metadata.uid,run:.metadata.labels."oberth.ci/run",phase:.status.phase}' >"$evidence/admitted-$phase-$expected.json"
    fixture_pod "admission-$phase-$expected" snapshot
    k -n oberth logs "admission-$phase-$expected" >"$evidence/admission-state-$phase-$expected.json"
    jq -e --arg id "$id" --arg job "$job" 'any(.Runs[]; .ID==$id and .Status=="running" and .Job==$job and .Started!=null)' "$evidence/admission-state-$phase-$expected.json" >/dev/null
    k -n oberth delete pod "admission-$phase-$expected" --wait=true
    k -n oberth-argo get pods -l "workflows.argoproj.io/workflow=$job" -o json | jq -e '.items|length==0' >/dev/null
    running_workflow=$job
  done
  k -n oberth scale deploy/oberth --replicas=0
  k -n oberth wait --for=delete pod -l app.kubernetes.io/name=oberth --timeout=100s
  fixture_pod "refill-$phase" refill
  k -n oberth delete pod "refill-$phase" --wait=true
  k -n oberth scale deploy/oberth --replicas=1
  k -n oberth rollout status deploy/oberth --timeout=150s
  snapshot "$phase-ready" "$key"
  # Compare the ledger across the deliberate queue mutations too. Strip only
  # the trailing run payload, retaining every integer timestamp byte for byte.
  for point in "$phase" "$phase-ready"; do
    sed -E 's/,"Runs":.*$/,"Runs":[],"Total":0}/' "$evidence/state-$point.json" >"$evidence/ledger-$point.json"
  done
  "$private/fixture-baseline" verify "$evidence/ledger-$phase.json" "$evidence/ledger-$phase-ready.json" \
    "$(jq '.Ledger|length' "$evidence/state-$phase.json")" 0
}
snapshot baseline
schedule_probe baseline baseline
previous=baseline-ready
for version in "${versions[@]}"; do
  started=$(date +%s%N)
  printf '%s\n' "$started" >"$evidence/upgrade-start-$version.txt"
  "$helm" upgrade oberth "$private/source-$version/charts/oberth" -n oberth --reuse-values \
    --set-string "image.ref=${candidate_refs[$version]}" --set image.pullPolicy=Never --wait --timeout 180s
  fixture_image="oberth-upgrade-e2e:$version-$$"
  expected_schema=$("$private/oberth-$version" version --schema)
  snapshot "$version"
  "$private/fixture-baseline" verify "$evidence/state-$previous.json" "$evidence/state-$version.json" "$expected_schema" "$started"
  for artifact in priorities workflow pod-priority; do
    ext=json; [[ $artifact != pod-priority ]] || ext=txt
    cmp "$evidence/$artifact-$previous.$ext" "$evidence/$artifact-$version.$ext"
  done
  schedule_probe "$version" "$version"
  # Restart the same target once more to detect migration replays or drift.
  k -n oberth rollout restart deploy/oberth
  k -n oberth rollout status deploy/oberth --timeout=180s
  snapshot "$version-restart"
  "$private/fixture-baseline" verify "$evidence/state-$version-ready.json" "$evidence/state-$version-restart.json" "$expected_schema" "$started"
  for artifact in priorities workflow pod-priority; do
    ext=json; [[ $artifact != pod-priority ]] || ext=txt
    cmp "$evidence/$artifact-$version-ready.$ext" "$evidence/$artifact-$version-restart.$ext"
  done
  schedule_probe "$version-restart" "$version"
  previous=$version-restart-ready
done
k -n oberth get secrets -o json | jq -e '[.items[]|select(.type!="helm.sh/release.v1")]|length==0' >/dev/null
printf 'PASS: release signature, behavioral red/green, five durable runs, ledger timestamps, priority classes, Workflow UID, upgrades, repeat restarts and live priority admission\n'
