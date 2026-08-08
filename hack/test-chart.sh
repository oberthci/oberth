#!/bin/sh
set -eu

chart=${1:-charts/oberth}
notes_template=$chart/templates/NOTES.txt
server_image=example.invalid/oberth@sha256:1111111111111111111111111111111111111111111111111111111111111111
runner_image=example.invalid/oberth-ci@sha256:2222222222222222222222222222222222222222222222222222222222222222

render() {
  helm template oberth "$chart" \
    --set "image.ref=$server_image" \
    --set "runnerImage.ref=$runner_image" \
    "$@"
}

helm lint "$chart" \
  --set "image.ref=$server_image" \
  --set "runnerImage.ref=$runner_image" >/dev/null

manifest=$(mktemp)
empty_release_manifest=$(mktemp)
generated_secrets_manifest=$(mktemp)
custom_anchor_manifest=$(mktemp)
package_dir=$(mktemp -d)
trap 'rm -f "$manifest" "$empty_release_manifest" "$generated_secrets_manifest" "$custom_anchor_manifest"; rm -rf -- "$package_dir"' EXIT
notes_chart=$package_dir/notes-chart
notes=$package_dir/notes.yaml
test -f "$notes_template"
test ! -L "$notes_template"
mkdir "$notes_chart"
cp -R -- "$chart"/. "$notes_chart"
awk '
  BEGIN {
    print "apiVersion: v1"
    print "kind: ConfigMap"
    print "metadata:"
    print "  name: oberth-notes-contract"
    print "data:"
    print "  notes: |-"
  }
  { print "    " $0 }
' "$notes_template" >"$notes_chart/templates/notes-contract.yaml"
helm template oberth "$notes_chart" --namespace default \
  --set "image.ref=$server_image" \
  --set "runnerImage.ref=$runner_image" \
  --show-only templates/notes-contract.yaml >"$notes"
render >"$manifest"
render --set-json 'releaseSecrets=[]' >"$empty_release_manifest"
render --set upstream.createSecrets=true --set ssh.hostKey.existingSecret= >"$generated_secrets_manifest"
render --set auditAnchor.rootsSecret=oberth-audit-tsa-roots >"$custom_anchor_manifest"
grep -q 'nodePort: 30022' "$manifest"
grep -q 'nodePort: 30443' "$manifest"
grep -q '^  name: oberth$' "$manifest"
grep -q '# Source: oberth/templates/tls-secret.yaml' "$manifest"
grep -q '^  name: oberth-tls$' "$manifest"
sed -n '/# Source: oberth\/templates\/pvc.yaml/,/^---$/p' "$manifest" | grep -q '^    helm.sh/resource-policy: keep$'
grep -q -- '- "oberth-upstream-key"' "$manifest"
grep -q -- '- "oberth-known-hosts"' "$manifest"
grep -q 'verbs: \["get", "patch"\]' "$manifest"
sed -n '/resources: \["pods"\]/,/verbs:/p' "$manifest" | grep -q 'verbs: \["get", "list", "watch", "patch"\]'
sed -n '/resources: \["configmaps"\]/,/verbs:/p' "$manifest" | grep -q 'verbs: \["create", "get", "list"\]'
if sed -n '/resources: \["configmaps"\]/,/verbs:/p' "$manifest" | grep -Eq '"update"|"patch"|"delete"'; then
  exit 1
fi
grep -q 'verbs: \["create"\]' "$manifest"
if grep -q 'verbs: \["create", "get", "delete"\]' "$manifest"; then
  exit 1
fi
test "$(grep -c '^            optional: true$' "$manifest")" -eq 2
grep -q '^            secretName: oberth-ssh-host-key$' "$manifest"
grep -q -- '--ssh-host-key=/etc/oberth/ssh/ssh_host_key' "$manifest"
grep -Fq -- '--job-ephemeral-storage-request=1Gi' "$manifest"
grep -Fq -- '--job-ephemeral-storage-limit=8Gi' "$manifest"
grep -Fq -- '- name: ci-cache' "$manifest"
grep -Fq -- 'mountPath: /var/cache/oberth/ci' "$manifest"
grep -Fq -- 'path: /var/cache/oberth/ci' "$manifest"
grep -Fq -- '- name: release-cache' "$manifest"
grep -Fq -- 'mountPath: /var/cache/oberth/release' "$manifest"
grep -Fq -- 'path: /var/cache/oberth/release' "$manifest"
# Cache roots are DirectoryOrCreate ONLY together with the prepare-caches init
# container that creates and owns them (0750, 65534) before the server starts;
# kubelet's root-owned 0755 auto-create alone is not the contract.
test "$(grep -c '^            type: DirectoryOrCreate$' "$manifest")" -ge 2
grep -Fq -- '- name: prepare-caches' "$manifest"
grep -Fq -- 'install -d -m 0750 -o 65534 -g 65534' "$manifest"
grep -Fq -- '--audit-tsa-url=https://timestamp.sectigo.com/rfc3161' "$manifest"
grep -Fq -- '--audit-rekor-url=https://rekor.sigstore.dev' "$manifest"
grep -Fq -- 'name: SIGSTORE_NO_CACHE' "$manifest"
grep -Fq -- 'value: "true"' "$manifest"
grep -Fq -- '--audit-anchor-interval=10m' "$manifest"
grep -Fq -- '--audit-anchor-max-age=30m' "$manifest"
if grep -Fq -- '--audit-tsa-roots=' "$manifest"; then
  exit 1
fi
grep -Fq -- '--audit-tsa-roots=/etc/oberth/audit-tsa/roots.pem' "$custom_anchor_manifest"
grep -q '^            secretName: oberth-audit-tsa-roots$' "$custom_anchor_manifest"
grep -q '^              ephemeral-storage: 256Mi$' "$manifest"
grep -q '^              ephemeral-storage: 2Gi$' "$manifest"
grep -q '^            sizeLimit: "2Gi"$' "$manifest"
grep -Fq -- '--namespace="default"' "$notes"
grep -Fq -- '--upstream-key-secret="oberth-upstream-key"' "$notes"
grep -Fq -- '"github" "ssh://git@github.com/your-org"' "$notes"
grep -Fq -- 'oberth uplink add - operator@host < ~/.ssh/id_ed25519.pub' "$notes"
grep -Fq -- '--cacert "$OBERTH_TLS_CERT"' "$notes"
grep -Fq -- '--resolve "oberth:${OBERTH_HTTPS_PORT}:${OBERTH_NODE_IP}"' "$notes"
grep -Fq -- 'Authorization: Bearer %s' "$notes"
grep -Fq -- '"method":"initialize"' "$notes"
grep -Fq -- '"method":"tools/list"' "$notes"
if grep -Eq 'curl[[:space:]]+(-[^[:space:]]*[[:space:]]+)*-k([[:space:]]|$)' "$notes"; then
  exit 1
fi
grep -Fq -- '-L oberth.ci/repo,oberth.ci/trigger,oberth.ci/burn,oberth.ci/step' "$notes"
if test "$(grep -c 'resources: \["secrets"\]' "$manifest")" -ne 3; then
	exit 1
fi
if test "$(grep -c 'resources: \["secrets"\]' "$empty_release_manifest")" -ne 2; then
  exit 1
fi
awk '
  /resources: \["secrets"\]/ { secret_rule = 1; named = 0; next }
  secret_rule && /resourceNames:/ { named = 1; next }
  secret_rule && /verbs:/ {
    if (!named && ($0 ~ /"get"/ || $0 ~ /"delete"/)) exit 1
    secret_rule = 0
  }
' "$manifest"
# Default values generate every identity Secret on first install (documented
# in NOTES); adopting restored credentials must render NO competing Secrets.
adopted_secrets_manifest=$(mktemp)
render --set upstream.createSecrets=false --set ssh.hostKey.existingSecret=restored-host-key >"$adopted_secrets_manifest"
if grep -q '# Source: oberth/templates/host-key-secret.yaml' "$adopted_secrets_manifest" || grep -q '# Source: oberth/templates/upstream-secrets.yaml' "$adopted_secrets_manifest"; then
  rm -f "$adopted_secrets_manifest"
  exit 1
fi
rm -f "$adopted_secrets_manifest"
grep -q '# Source: oberth/templates/host-key-secret.yaml' "$generated_secrets_manifest"
grep -q '# Source: oberth/templates/upstream-secrets.yaml' "$generated_secrets_manifest"
grep -q '^  name: oberth-ssh-host-key$' "$generated_secrets_manifest"
grep -q '^  ssh_host_key:' "$generated_secrets_manifest"
grep -q '^  name: oberth-upstream-key$' "$generated_secrets_manifest"
grep -q '^  name: oberth-known-hosts$' "$generated_secrets_manifest"
awk '
  /^# Source:/ { upstream = ($0 == "# Source: oberth/templates/upstream-secrets.yaml") }
  upstream && /^    helm.sh\/resource-policy: keep$/ { kept++ }
  END { exit kept == 2 ? 0 : 1 }
' "$generated_secrets_manifest"

if render --set replicaCount=2 >/dev/null 2>&1; then
  exit 1
fi
if render --set cache.ciRoot=/var/cache/oberth --set cache.releaseRoot=/var/cache/oberth/release >/dev/null 2>&1; then
  exit 1
fi
if render --set service.sshNodePort=32022 >/dev/null 2>&1; then
  exit 1
fi
if render --set replciaCount=1 >/dev/null 2>&1; then
  exit 1
fi
if render --set service.htpsNodePort=31443 >/dev/null 2>&1; then
  exit 1
fi
if render --set maxConcurrentJobs=65 >/dev/null 2>&1; then
  exit 1
fi
if render --set jobs.ttlSecondsAfterFinished=0 >/dev/null 2>&1; then
  exit 1
fi
if render --set jobs.timeout=eventually >/dev/null 2>&1; then
  exit 1
fi
if render --set upstream.keyFile=../escape >/dev/null 2>&1; then
  exit 1
fi
if render --set upstream.knownHostsKey=/escape >/dev/null 2>&1; then
  exit 1
fi
if render --set upstream.keyFile=. >/dev/null 2>&1; then
  exit 1
fi
if render --set upstream.keySecret=shared --set upstream.knownHostsSecret=shared >/dev/null 2>&1; then
  exit 1
fi
if render --set persistence.size=9Gi >/dev/null 2>&1; then
  exit 1
fi
if render --set persistence.size=1Mi >/dev/null 2>&1; then
  exit 1
fi
if render --set cache.ciRoot=/var/cache/oberth/ci/ >/dev/null 2>&1; then
  exit 1
fi
if render --set cache.ciRoot=/var/cache/oberth/../ci >/dev/null 2>&1; then
  exit 1
fi
if render --set-json 'releaseSecrets=["BAD_NAME"]' >/dev/null 2>&1; then
  exit 1
fi
if render --set-json 'releaseSecrets=["gar..key"]' >/dev/null 2>&1; then
  exit 1
fi
if render --set-json 'releaseSecrets=["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.example"]' >/dev/null 2>&1; then
  exit 1
fi
for audit_url in \
  'http://audit.example.test' \
  'https:///missing-host' \
  'HTTPS://audit.example.test' \
  'https://user:pass@audit.example.test' \
  'https://audit.example.test/path#fragment' \
  'https://audit.example.test/path#'; do
  for audit_key in tsaURL rekorURL; do
    if render --set-string "auditAnchor.${audit_key}=${audit_url}" >/dev/null 2>&1; then
      exit 1
    fi
  done
done

helm package "$chart" --destination "$package_dir" >/dev/null
set -- "$package_dir"/oberth-*.tgz
test "$#" -eq 1
tar -tzf "$1" | grep -q '^oberth/files/prepare-node.sh$'
mkdir "$package_dir/extracted"
tar -xzf "$1" -C "$package_dir/extracted"
sh "$package_dir/extracted/oberth/files/prepare-node.sh" \
  --ci-root "$package_dir/cache-ci" \
  --release-root "$package_dir/cache-release" \
  --uid "$(id -u)" \
  --gid "$(id -g)"
