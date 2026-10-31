#!/usr/bin/env bash
# Disposable local kind fixture; never uses the caller's current kube context.
# Synthetic kubelet disk threshold, no disk filling or host memory pressure.
set -euo pipefail
task_name="oberth-platform-r2"
task_root="${1:?usage: qualify-platform-pressure.sh /absolute/evidence-directory}"
case "$task_root" in /*) ;; *) echo 'evidence directory must be absolute' >&2; exit 1;; esac
mkdir -p "$task_root"
task_kubeconfig="$task_root/kubeconfig"
task_node="$task_name-control-plane"
task_namespace=platform-qualification
task_image=kindest/node:v1.36.4
k() { kubectl --kubeconfig "$task_kubeconfig" --context "kind-$task_name" "$@"; }
if kind get clusters 2>/dev/null | grep -qx "$task_name"; then
  echo 'refusing to reuse an existing qualification cluster' >&2
  exit 1
fi
task_created=false
cleanup() {
  task_status=$?
  if "$task_created"; then
    k get node "$task_node" -o json > "$task_root/node-final.json" 2>/dev/null || true
    k -n "$task_namespace" get pods,events -o json > "$task_root/pods-events-final.json" 2>/dev/null || true
    docker exec "$task_node" journalctl -u kubelet --no-pager -n 80 > "$task_root/kubelet-final.log" 2>&1 || true
    kind delete cluster --name "$task_name" --kubeconfig "$task_kubeconfig" > "$task_root/delete.log" 2>&1 || true
  fi
  exit "$task_status"
}
trap cleanup EXIT
docker image inspect "$task_image" > "$task_root/node-image.json"
task_created=true
kind create cluster --name "$task_name" --image "$task_image" --kubeconfig "$task_kubeconfig" --wait 60s > "$task_root/create.log" 2>&1
k wait --for=condition=Ready "node/$task_node" --timeout=90s
k create namespace "$task_namespace"
docker exec "$task_node" crictl images -o json > "$task_root/images.json"
task_pause=$(python3 - "$task_root/images.json" <<'PY'
import json,sys
for image in json.load(open(sys.argv[1]))['images']:
    for tag in image.get('repoTags',[]):
        if '/pause:' in tag:
            print(tag);sys.exit(0)
raise SystemExit('cached pause image missing; no image download is permitted')
PY
)
pod_json() {
  python3 - "$1" "$2" "$3" "$4" "$task_pause" <<'PY'
import json,sys
name,priority,tolerate,resources,image=sys.argv[1:]
spec={'restartPolicy':'Never','terminationGracePeriodSeconds':0,
      'automountServiceAccountToken':False,
      'containers':[{'name':'main','image':image,'imagePullPolicy':'Never'}]}
if priority:spec['priorityClassName']=priority
if tolerate=='yes':spec['tolerations']=[{'key':'node.kubernetes.io/'+p+'-pressure','operator':'Exists','effect':'NoSchedule'} for p in ('memory','disk')]
if resources=='yes':spec['containers'][0]['resources']={'requests':{'cpu':'10m','memory':'16Mi'},'limits':{'cpu':'10m','memory':'16Mi'}}
print(json.dumps({'apiVersion':'v1','kind':'Pod','metadata':{'name':name},'spec':spec}))
PY
}
create_pod() { pod_json "$@" | k -n "$task_namespace" create -f -; }
wait_scheduled_false() {
  local task_pod=$1
  for _task_tick in {1..30}; do
    if [[ "$(k -n "$task_namespace" get pod "$task_pod" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].reason}')" == Unschedulable ]]; then return; fi
    sleep 1
  done
  echo "negative control $task_pod was not Unschedulable" >&2; exit 1
}
create_pod ordinary-class oberth-placeholder no yes >/dev/null 2>&1 && {
  echo 'unexpected unregistered PriorityClass acceptance' >&2; exit 1;
}
cat > "$task_root/priority.json" <<'JSON'
{"apiVersion":"scheduling.k8s.io/v1","kind":"PriorityClass","metadata":{"name":"oberth-placeholder"},"value":1000000,"globalDefault":false,"preemptionPolicy":"PreemptLowerPriority"}
JSON
k create -f "$task_root/priority.json"
pod_json dry-run-critical system-cluster-critical yes yes | k -n "$task_namespace" create --dry-run=server -f - -o json > "$task_root/namespace-dry-run.json"
python3 - "$task_root/namespace-dry-run.json" <<'PY'
import json,sys
p=json.load(open(sys.argv[1]));assert p['metadata']['namespace']=='platform-qualification' and p['spec']['priority']==2000000000
PY
echo 'PASS critical class admitted outside kube-system in vanilla 1.36.4'

k get --raw "/api/v1/nodes/$task_node/proxy/configz" > "$task_root/configz-before.json"
docker cp "$task_node:/var/lib/kubelet/config.yaml" "$task_root/kubelet-before.yaml"
if docker exec "$task_node" test -f /etc/default/kubelet; then
  docker cp "$task_node:/etc/default/kubelet" "$task_root/kubelet-default-before"
fi
pressure_config() {
  python3 - "$task_root/configz-before.json" "$task_root/kubelet-pressure.json" "$1" <<'PY'
import json,sys
c=json.load(open(sys.argv[1]))['kubeletconfig']
c.update(apiVersion='kubelet.config.k8s.io/v1beta1',kind='KubeletConfiguration')
key='memory.available' if sys.argv[3]=='memory' else 'nodefs.available'
# 100% is ignored; a hard floor above capacity makes node allocatable invalid.
# A soft threshold reports the real condition before grace without shrinking
# allocatable, filling disk, allocating memory or evicting any fixture pods.
c['evictionSoft']={key:'100Ti'}
c['evictionSoftGracePeriod']={key:'1h'}
c['evictionPressureTransitionPeriod']='10s'
json.dump(c,open(sys.argv[2],'w'))
PY
  docker cp "$task_root/kubelet-pressure.json" "$task_node:/var/lib/kubelet/config.yaml"
  # kind supplies eviction CLI overrides. The final extra-args flag must agree
  # with the fixture file; otherwise changing the file does not create pressure.
  local task_hard
  task_hard=$(python3 - "$task_root/kubelet-pressure.json" <<'PY'
import json,sys
print(','.join(k+'<'+v for k,v in sorted(json.load(open(sys.argv[1]))['evictionHard'].items())))
PY
)
  docker exec "$task_node" sh -c 'test ! -f /etc/default/kubelet || . /etc/default/kubelet; printf "\nKUBELET_EXTRA_ARGS=\"%s --eviction-hard=%s\"\n" "${KUBELET_EXTRA_ARGS-}" "$1" >> /etc/default/kubelet' sh "$task_hard"
  docker exec "$task_node" systemctl restart kubelet
  local task_deadline=$((SECONDS+45))
  until k --request-timeout=5s get --raw "/api/v1/nodes/$task_node/proxy/configz" > "$task_root/configz-$1.json"; do
    if ((SECONDS>=task_deadline)); then echo 'kubelet configz unavailable after fixture restart' >&2; exit 1; fi
    sleep 1
  done
  python3 - "$task_root/configz-$1.json" "$1" <<'PY'
import json,sys
c=json.load(open(sys.argv[1]))['kubeletconfig'];key='memory.available' if sys.argv[2]=='memory' else 'nodefs.available'
assert c['evictionSoft'][key]=='100Ti',c['evictionSoft']
assert c['evictionSoftGracePeriod'][key]=='1h'
PY
}
restore_config() {
  docker cp "$task_root/kubelet-before.yaml" "$task_node:/var/lib/kubelet/config.yaml"
  if [[ -f "$task_root/kubelet-default-before" ]]; then
    docker cp "$task_root/kubelet-default-before" "$task_node:/etc/default/kubelet"
  else
    docker exec "$task_node" sh -c ': > /etc/default/kubelet'
  fi
  docker exec "$task_node" systemctl restart kubelet
}
wait_taint() {
  local task_key=$1
  for _task_tick in {1..60}; do
    if k get node "$task_node" -o json | python3 -c 'import json,sys; n=json.load(sys.stdin);sys.exit(0 if any(t["key"]==sys.argv[1] and t["effect"]=="NoSchedule" for t in n["spec"].get("taints",[])) else 1)' "$task_key"; then return; fi
    sleep 1
  done
  echo "missing pressure taint $task_key" >&2; exit 1
}
# A manually written pressure taint is removed by the node controller while the
# real condition is false. Keep the synthetic condition true instead, without
# allocating memory or filling disk on the host.
pressure_config memory
k wait --for=condition=MemoryPressure "node/$task_node" --timeout=90s
wait_taint node.kubernetes.io/memory-pressure
create_pod blocked-memory '' no no
wait_scheduled_false blocked-memory
create_pod tolerant-memory system-cluster-critical yes yes
k -n "$task_namespace" wait --for=condition=Ready pod/tolerant-memory --timeout=60s
k -n "$task_namespace" get pod tolerant-memory -o json > "$task_root/taint-memory.json"
k -n "$task_namespace" delete pod blocked-memory tolerant-memory --wait=true
restore_config
k wait --for=condition=MemoryPressure=false "node/$task_node" --timeout=120s
echo 'PASS synthetic MemoryPressure scheduler positive/negative controls'

for task_app in oberth argo openbao cloudflared; do
  create_pod "$task_app" system-cluster-critical yes yes
  k -n "$task_namespace" wait --for=condition=Ready "pod/$task_app" --timeout=60s
done
k -n "$task_namespace" get pods -o json > "$task_root/before-pressure.json"
pressure_config disk
k wait --for=condition=DiskPressure "node/$task_node" --timeout=90s
wait_taint node.kubernetes.io/disk-pressure
create_pod blocked-disk '' no no
wait_scheduled_false blocked-disk
k -n "$task_namespace" delete pod blocked-disk --wait=true
echo 'PASS DiskPressure scheduler negative control'
create_pod tolerant-ordinary oberth-placeholder yes yes
for _task_tick in {1..60}; do
  task_verdict=$(k -n "$task_namespace" get pod tolerant-ordinary -o jsonpath='{.status.reason}')
  if [[ "$task_verdict" == Evicted ]]; then break; fi
  sleep 1
done
[[ "$task_verdict" == Evicted ]] || { echo 'ordinary tolerant pod was not rejected by kubelet' >&2; exit 1; }
k -n "$task_namespace" get pod tolerant-ordinary -o json > "$task_root/ordinary-kubelet-rejection.json"
echo 'PASS tolerant custom-priority control rejected by real kubelet DiskPressure'
for task_app in oberth argo openbao cloudflared; do
  k -n "$task_namespace" delete pod "$task_app" --wait=true
  create_pod "$task_app" system-cluster-critical yes yes
  k -n "$task_namespace" wait --for=condition=Ready "pod/$task_app" --timeout=60s
done
k -n "$task_namespace" get pods -o json > "$task_root/recreated-under-pressure.json"
python3 - "$task_root/before-pressure.json" "$task_root/recreated-under-pressure.json" <<'PY'
import json,sys
old={p['metadata']['name']:p for p in json.load(open(sys.argv[1]))['items']}
new={p['metadata']['name']:p for p in json.load(open(sys.argv[2]))['items']}
for name in ('oberth','argo','openbao','cloudflared'):
    p=new[name]
    assert p['metadata']['uid']!=old[name]['metadata']['uid']
    assert p['spec']['priority']==2000000000 and p['status']['qosClass']=='Guaranteed'
    assert any(c['type']=='Ready' and c['status']=='True' for c in p['status']['conditions'])
print('PASS four stand-in replacements: new UIDs, critical priority, Guaranteed and Ready under real DiskPressure')
PY
restore_config
k wait --for=condition=DiskPressure=false "node/$task_node" --timeout=120s
echo 'PASS pressure cleared after exact kubelet configuration restore'
echo 'Stand-ins test scheduler/kubelet admission, not actual OpenBao unseal, DB, token delivery or ingress.'
