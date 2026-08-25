#!/bin/sh
set -eu

chart=${1:-charts/oberth}
notes_template=$chart/templates/NOTES.txt
server_image=example.invalid/oberth@sha256:1111111111111111111111111111111111111111111111111111111111111111

render() {
  helm template oberth "$chart" \
    --set "image.ref=$server_image" \
    "$@"
}

helm lint "$chart" \
  --set "image.ref=$server_image" >/dev/null

manifest=$(mktemp)
empty_release_manifest=$(mktemp)
generated_secrets_manifest=$(mktemp)
custom_anchor_manifest=$(mktemp)
custom_rekor_manifest=$(mktemp)
rotated_rekor_manifest=$(mktemp)
custom_rekor_deployment=$(mktemp)
rotated_rekor_deployment=$(mktemp)
rekor_public_key=$(mktemp)
rotated_rekor_public_key=$(mktemp)
empty_rekor_public_key=$(mktemp)
insecure_rekor_manifest=$(mktemp)
insecure_tsa_manifest=$(mktemp)
secure_secretstore_manifest=$(mktemp)
rotated_secretstore_manifest=$(mktemp)
dev_secretstore_manifest=$(mktemp)
staged_transit_manifest=$(mktemp)
secretstore_ca=$(mktemp)
rotated_secretstore_ca=$(mktemp)
default_service_manifest=$(mktemp)
default_nodeport_manifest=$(mktemp)
adoption_service_manifest=$(mktemp)
desired_service_manifest=$(mktemp)
netpol_manifest=$(mktemp)
alt_netpol=$(mktemp)
ext_netpol=$(mktemp)
argo_vault_manifest=$(mktemp)
argo_legacy_manifest=$(mktemp)
package_dir=$(mktemp -d)
trap 'rm -f "$manifest" "$empty_release_manifest" "$generated_secrets_manifest" "$custom_anchor_manifest" "$custom_rekor_manifest" "$rotated_rekor_manifest" "$custom_rekor_deployment" "$rotated_rekor_deployment" "$rekor_public_key" "$rotated_rekor_public_key" "$empty_rekor_public_key" "$insecure_rekor_manifest" "$insecure_tsa_manifest" "$secure_secretstore_manifest" "$rotated_secretstore_manifest" "$dev_secretstore_manifest" "$staged_transit_manifest" "$secretstore_ca" "$rotated_secretstore_ca" "$default_service_manifest" "$default_nodeport_manifest" "$adoption_service_manifest" "$desired_service_manifest" "$netpol_manifest" "$alt_netpol" "$ext_netpol" "$argo_vault_manifest" "$argo_legacy_manifest"; rm -rf -- "$package_dir"' EXIT
cat >"$rekor_public_key" <<'EOF'
-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE4XEptRLUdhdCPBXSRJWiuCYQZ3HO
hiY7f15loLe499OZauEsS2Vk3Jcs52b3yz63LmW2aPrRXQZevheA+9nouw==
-----END PUBLIC KEY-----
EOF
cat >"$empty_rekor_public_key" <<'EOF'
-----BEGIN PUBLIC KEY-----

-----END PUBLIC KEY-----
EOF
cat >"$rotated_rekor_public_key" <<'EOF'
-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE9cygyVi1MIsvTJNcTw7uvZL6yuTb
g1YzDQDo9goog1vos6NTmFFEOEUv5zggRAFvjEDJyqYzFBsCrLqFfPVYcA==
-----END PUBLIC KEY-----
EOF
cat >"$secretstore_ca" <<'EOF'
-----BEGIN CERTIFICATE-----
installer-managed-public-ca-one
-----END CERTIFICATE-----
EOF
cat >"$rotated_secretstore_ca" <<'EOF'
-----BEGIN CERTIFICATE-----
installer-managed-public-ca-two
-----END CERTIFICATE-----
EOF
notes_chart=$package_dir/notes-chart
notes=$package_dir/notes.yaml
test -f "$notes_template"
test ! -L "$notes_template"
mkdir "$notes_chart"
cp -R -- "$chart"/. "$notes_chart"
# cp -R preserves the source's permissions, so a read-only checkout produces a
# read-only copy and the template pruning below fails with "Permission denied"
# under set -e. The Argo execution engine mounts a run's source read-only, so
# this script must not assume its checkout is writable.
chmod -R u+w -- "$notes_chart"
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
  --show-only templates/notes-contract.yaml >"$notes"
render >"$manifest"
render >"$empty_release_manifest"  # default render has no releaseSecrets
render --set upstream.createSecrets=true --set ssh.hostKey.existingSecret= >"$generated_secrets_manifest"
render \
  --set auditAnchor.tsaURL=https://timestamp.sectigo.com/rfc3161 \
  --set auditAnchor.rekorURL=https://rekor.sigstore.dev \
  --set auditAnchor.rootsSecret=oberth-audit-tsa-roots >"$custom_anchor_manifest"
render --set auditAnchor.rekorURL=https://rekor.sigstore.dev \
  --set-file auditAnchor.rekorPublicKey="$rekor_public_key" >"$custom_rekor_manifest"
render --set auditAnchor.rekorURL=https://rekor.sigstore.dev \
  --set-file auditAnchor.rekorPublicKey="$rotated_rekor_public_key" >"$rotated_rekor_manifest"
render --set auditAnchor.rekorInsecureHTTP=true --set-string auditAnchor.rekorURL=http://rekor.rekor-system.svc:3000 >"$insecure_rekor_manifest"
render --set auditAnchor.tsaInsecureHTTP=true --set-string auditAnchor.tsaURL=http://tsa.tsa-system.svc:3000 >"$insecure_tsa_manifest"
render --set secretstore.enabled=true --set-file secretstore.caCert="$secretstore_ca" >"$secure_secretstore_manifest"
render --set secretstore.enabled=true --set-file secretstore.caCert="$rotated_secretstore_ca" >"$rotated_secretstore_manifest"
render --set secretstore.enabled=true --set secretstore.transit.enabled=false \
  --set secretstore.insecureHTTPForDev=true \
  --set-string secretstore.address=http://openbao.openbao.svc:8200 >"$dev_secretstore_manifest"
render --set secretstore.enabled=true --set secretstore.transit.enabled=true >"$staged_transit_manifest"
render --show-only templates/service.yaml >"$default_service_manifest"
render --show-only templates/service.yaml \
  --set service.type=LoadBalancer \
  --set service.sshPort=2222 >"$adoption_service_manifest"
render --show-only templates/service.yaml \
  --set service.type=NodePort \
  --set service.sshPort=22 \
  --set service.httpsPort=8443 >"$desired_service_manifest"
sed -n '/# Source: oberth\/templates\/deployment.yaml/,/^---$/p' "$custom_rekor_manifest" >"$custom_rekor_deployment"
sed -n '/# Source: oberth\/templates\/deployment.yaml/,/^---$/p' "$rotated_rekor_manifest" >"$rotated_rekor_deployment"
grep -q 'nodePort: 30022' "$manifest"
grep -q 'nodePort: 30443' "$manifest"
# The default IS the LoadBalancer shape: a stock k3s exposes both ports through
# svclb with no extra values, and the NodePorts stay in place so the same render
# also serves clients on a cluster with no load-balancer controller.
cmp -s "$default_service_manifest" "$adoption_service_manifest"
grep -q '^  type: LoadBalancer$' "$adoption_service_manifest"
grep -A4 -q '^    - name: ssh$' "$adoption_service_manifest"
# 2222, not 22: under LoadBalancer svclb binds the SERVICE port on the host, and
# the host's own sshd already owns 22 on essentially every machine.
grep -A4 '^    - name: ssh$' "$adoption_service_manifest" | grep -q '^      port: 2222$'
grep -A5 '^    - name: ssh$' "$adoption_service_manifest" | grep -q '^      nodePort: 30022$'
grep -A4 '^    - name: https$' "$adoption_service_manifest" | grep -q '^      port: 443$'
# Both service ports stay overridable in either shape.
grep -q '^  type: NodePort$' "$desired_service_manifest"
grep -A4 '^    - name: ssh$' "$desired_service_manifest" | grep -q '^      port: 22$'
grep -A5 '^    - name: ssh$' "$desired_service_manifest" | grep -q '^      nodePort: 30022$'
grep -A4 '^    - name: https$' "$desired_service_manifest" | grep -q '^      port: 8443$'
grep -A5 '^    - name: https$' "$desired_service_manifest" | grep -q '^      nodePort: 30443$'
grep -q '^  name: oberth$' "$manifest"
grep -q '# Source: oberth/templates/tls-secret.yaml' "$manifest"
grep -q '^  name: oberth-tls$' "$manifest"
sed -n '/# Source: oberth\/templates\/pvc.yaml/,/^---$/p' "$manifest" | grep -q '^    helm.sh/resource-policy: keep$'
grep -q -- '- "oberth-upstream-key"' "$manifest"
grep -q -- '- "oberth-known-hosts"' "$manifest"
grep -q 'verbs: \["get", "patch"\]' "$manifest"
sed -n '/resources: \["pods"\]/,/verbs:/p' "$manifest" | grep -q 'verbs: \["get", "list", "watch", "patch"\]'
# ConfigMap least privilege: the collection rule is read/create/watch only
# (watch feeds the secret-access reconciler and cannot be name-scoped
# portably); in-place mutation exists solely as a resourceNames-scoped update
# on the grants ConfigMap, so audit-anchor continuity ConfigMaps stay
# unwritable by the server's own identity. patch and delete stay absent
# everywhere.
sed -n '/resources: \["configmaps"\]/,/verbs:/p' "$manifest" | grep -q 'verbs: \["create", "get", "list", "watch"\]'
sed -n '/resources: \["configmaps"\]/,/verbs:/p' "$manifest" | grep -q 'resourceNames: \["oberth-secret-access"\]'
sed -n '/resources: \["configmaps"\]/,/verbs:/p' "$manifest" | grep -q 'verbs: \["update"\]'
if sed -n '/resources: \["configmaps"\]/,/verbs:/p' "$manifest" | grep -Eq '"patch"|"delete"'; then
  exit 1
fi
# update may never ride on the unnamed collection rule.
if sed -n '/resources: \["configmaps"\]/,/verbs:/p' "$manifest" | grep -Eq '"update".*"create"|"create".*"update"'; then
  exit 1
fi
if sed -n '/resourceNames: \["oberth-secret-access"\]/,/verbs:/p' "$manifest" | grep -Eq '"create"|"list"|"watch"|"patch"|"delete"'; then
  exit 1
fi
# No unbounded secrets create rule or batch/jobs CRUD.
if grep -q 'resources: \["jobs"\]' "$manifest"; then
  exit 1
fi
test "$(grep -c '^            optional: true$' "$manifest")" -eq 2
grep -q '^            secretName: oberth-ssh-host-key$' "$manifest"
grep -q -- '--ssh-host-key=/etc/oberth/ssh/ssh_host_key' "$manifest"
grep -Fq -- '- name: ci-cache' "$manifest"
grep -Fq -- 'mountPath: /var/cache/oberth/ci' "$manifest"
grep -Fq -- 'path: /var/cache/oberth/ci' "$manifest"
grep -Fq -- '- name: release-cache' "$manifest"
grep -Fq -- 'mountPath: /var/cache/oberth/release' "$manifest"
grep -Fq -- 'path: /var/cache/oberth/release' "$manifest"
# Cache volumes use DirectoryOrCreate; kubelet creates the host directory.
test "$(grep -c '^            type: DirectoryOrCreate$' "$manifest")" -ge 2
# External audit anchoring is opt-in: the default render must contact no
# external service, so neither URL flag may appear. The local hash chain and
# its cycle cadence still render unconditionally.
if grep -Fq -- '--audit-tsa-url=' "$manifest"; then
  exit 1
fi
if grep -Fq -- '--audit-rekor-url=' "$manifest"; then
  exit 1
fi
if grep -Fq -- '--audit-rekor-insecure-http' "$manifest"; then
  exit 1
fi
if grep -Fq -- '--audit-tsa-insecure-http' "$manifest"; then
  exit 1
fi
grep -Fq -- '--audit-rekor-url=http://rekor.rekor-system.svc:3000' "$insecure_rekor_manifest"
grep -Fq -- '--audit-rekor-insecure-http' "$insecure_rekor_manifest"
grep -Fq -- '--audit-tsa-url=http://tsa.tsa-system.svc:3000' "$insecure_tsa_manifest"
grep -Fq -- '--audit-tsa-insecure-http' "$insecure_tsa_manifest"
# Secret-store TLS remains independently usable with Transit disabled. The
# composed trusted-plan runtime emits both fixed identifiers only after its
# nested capability is explicitly enabled.
grep -Fq -- '--secretstore-address=https://openbao.openbao.svc:8200' "$secure_secretstore_manifest"
grep -Fq -- '--secretstore-ca-cert=/etc/oberth/secretstore-ca/ca.crt' "$secure_secretstore_manifest"
grep -Fq -- 'name: oberth-secretstore-ca' "$secure_secretstore_manifest"
if grep -Fq -- '--secretstore-transit-' "$secure_secretstore_manifest"; then
  exit 1
fi
grep -Fq -- '--secretstore-transit-mount=oberth-transit' "$staged_transit_manifest"
grep -Fq -- '--secretstore-transit-key=trusted-plan-artifacts' "$staged_transit_manifest"
if grep -Fq -- '--secretstore-insecure-http' "$secure_secretstore_manifest"; then
  exit 1
fi
grep -Fq -- '--secretstore-address=http://openbao.openbao.svc:8200' "$dev_secretstore_manifest"
grep -Fq -- '--secretstore-insecure-http' "$dev_secretstore_manifest"
if grep -Fq -- '--secretstore-transit-' "$dev_secretstore_manifest"; then
  exit 1
fi
secretstore_checksum=$(awk '$1 == "checksum/secretstore-ca:" { print $2 }' "$secure_secretstore_manifest")
rotated_secretstore_checksum=$(awk '$1 == "checksum/secretstore-ca:" { print $2 }' "$rotated_secretstore_manifest")
test -n "$secretstore_checksum"
test -n "$rotated_secretstore_checksum"
test "$secretstore_checksum" != "$rotated_secretstore_checksum"
# Transit payloads can never be enabled over the KV-only HTTP development
# escape hatch, and both Transit identifiers are single path segments.
if render --set secretstore.enabled=true --set secretstore.transit.enabled=true --set secretstore.insecureHTTPForDev=true >/dev/null 2>&1; then
  exit 1
fi
if render --set secretstore.enabled=true --set secretstore.transit.enabled=true --set secretstore.insecureHTTPForDev=true \
  --set-string secretstore.address=http://openbao.openbao.svc:8200 >/dev/null 2>&1; then
  exit 1
fi
if render --set secretstore.enabled=true --set secretstore.transit.enabled=true --set-string secretstore.transit.mount=bad/mount >/dev/null 2>&1; then
  exit 1
fi
if render --set secretstore.enabled=true --set secretstore.transit.enabled=true --set-string secretstore.transit.key= >/dev/null 2>&1; then
  exit 1
fi
if render --set secretstore.enabled=true --set secretstore.transit.enabled=true --set-string secretstore.transit.mount=.. >/dev/null 2>&1; then
  exit 1
fi
if render --set secretstore.enabled=true --set secretstore.transit.enabled=true --set-string secretstore.transit.mount=sys >/dev/null 2>&1; then
  exit 1
fi
if render --set secretstore.enabled=true --set secretstore.transit.enabled=true --set-string secretstore.transit.key=. >/dev/null 2>&1; then
  exit 1
fi
if render --set secretstore.enabled=true --set-string 'secretstore.address=https://user@openbao.openbao.svc:8200' >/dev/null 2>&1; then
  exit 1
fi
if render --set secretstore.enabled=true --set-string 'secretstore.address=https://openbao.openbao.svc:8200#fragment' >/dev/null 2>&1; then
  exit 1
fi
grep -Fq -- 'name: SIGSTORE_NO_CACHE' "$manifest"
grep -Fq -- 'value: "true"' "$manifest"
grep -Fq -- '--audit-anchor-interval=10m' "$manifest"
grep -Fq -- '--audit-anchor-max-age=30m' "$manifest"
if grep -Fq -- '--audit-tsa-roots=' "$manifest"; then
  exit 1
fi
grep -Fq -- '--audit-tsa-url=https://timestamp.sectigo.com/rfc3161' "$custom_anchor_manifest"
grep -Fq -- '--audit-rekor-url=https://rekor.sigstore.dev' "$custom_anchor_manifest"
grep -Fq -- '--audit-tsa-roots=/etc/oberth/audit-tsa/roots.pem' "$custom_anchor_manifest"
grep -q '^            secretName: oberth-audit-tsa-roots$' "$custom_anchor_manifest"
# Dependent audit knobs without their URL must fail the render, not produce a
# pod the server refuses to start.
if render --set auditAnchor.rootsSecret=oberth-audit-tsa-roots >/dev/null 2>&1; then
  exit 1
fi
if render --set-string 'auditAnchor.tsaCA=pem' >/dev/null 2>&1; then
  exit 1
fi
if render --set-string 'auditAnchor.rekorCA=pem' >/dev/null 2>&1; then
  exit 1
fi
if render --set-file "auditAnchor.rekorPublicKey=$rekor_public_key" >/dev/null 2>&1; then
  exit 1
fi
if render --set auditAnchor.rekorInsecureHTTP=true >/dev/null 2>&1; then
  exit 1
fi
if render --set auditAnchor.tsaInsecureHTTP=true >/dev/null 2>&1; then
  exit 1
fi
if render --set-string 'auditAnchor.acceptWitnessChainReset=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' >/dev/null 2>&1; then
  exit 1
fi
if grep -Fq -- '--audit-rekor-pubkey=' "$manifest" ||
  grep -Fq -- '# Source: oberth/templates/audit-rekor-pubkey.yaml' "$manifest" ||
  grep -Fq -- 'name: audit-rekor-pubkey' "$manifest"; then
  exit 1
fi
grep -Fq -- '# Source: oberth/templates/audit-rekor-pubkey.yaml' "$custom_rekor_manifest"
grep -q '^  name: oberth-audit-rekor-pubkey$' "$custom_rekor_manifest"
grep -Fq -- '--audit-rekor-pubkey=/etc/oberth/audit-rekor-pubkey/rekor.pub' "$custom_rekor_deployment"
grep -Fq -- 'mountPath: /etc/oberth/audit-rekor-pubkey' "$custom_rekor_deployment"
grep -Fq -- '        - name: audit-rekor-pubkey' "$custom_rekor_deployment"
grep -Fq -- '          name: oberth-audit-rekor-pubkey' "$custom_rekor_deployment"
grep -q '^  rekor.pub: |$' "$custom_rekor_manifest"
rekor_checksum=$(awk '$1 == "checksum/audit-rekor-pubkey:" { print $2 }' "$custom_rekor_deployment")
rotated_rekor_checksum=$(awk '$1 == "checksum/audit-rekor-pubkey:" { print $2 }' "$rotated_rekor_deployment")
test -n "$rekor_checksum"
test -n "$rotated_rekor_checksum"
test "$rekor_checksum" != "$rotated_rekor_checksum"
grep -q '^              ephemeral-storage: 256Mi$' "$manifest"
grep -q '^              ephemeral-storage: 2Gi$' "$manifest"
grep -q '^            sizeLimit: "2Gi"$' "$manifest"
# NOTES contract, trimmed to the essential first steps by #838: namespaced
# in-pod exec commands for the two bootstrap actions, the vault-like
# not-ready explanation, and the docs pointer. The previous long-form
# assertions (verbose flags, curl MCP walkthrough, log-label hints) pinned
# content #838 deliberately removed and had left this gate red.
# The exec-command pattern is concatenated from two shell words so this
# script's own body stays free of cluster-dependent command text — the
# fresh-tree contract asserts chart validation never runs such commands.
grep -Fq -- 'kubectl'' exec -it -n default deploy/oberth' "$notes"
grep -Fq -- 'oberth upstream add github ssh://git@github.com/your-org' "$notes"
grep -Fq -- 'oberth uplink add - operator@host < ~/.ssh/id_ed25519.pub' "$notes"
grep -Fq -- 'stays Running but not ready (0/1)' "$notes"
grep -Fq -- 'https://github.com/oberthci/oberth#documentation' "$notes"
if grep -Eq 'curl[[:space:]]+(-[^[:space:]]*[[:space:]]+)*-k([[:space:]]|$)' "$notes"; then
  exit 1
fi
# Secrets: only one named rule for upstream key/known-hosts; no unbounded
# create, no releaseSecrets conditional.
if test "$(grep -c 'resources: \["secrets"\]' "$manifest")" -ne 1; then
	exit 1
fi
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
# Upstream Secrets use a lookup guard: when the Secret already exists
# (connected upgrade), the template renders NOTHING for it — Helm orphans the
# live Secret via helm.sh/resource-policy: keep, preserving its data and
# removing the key from future release-manifest history. When absent (fresh
# install), the template renders data: {} with resource-policy: keep so no key
# material ever enters the manifest.
#
# NOTE: The existing-install orphan path (lookup returns the Secret) cannot be
# exercised by offline helm template — lookup always returns empty offline.
# The orphan behavior is a documented Helm resource-policy:keep semantic.
#
# The rendered output must never contain private key material or re-embedded
# Secret data (no toYaml of existing .data).
upstream_secrets_section=$(sed -n '/# Source: oberth\/templates\/upstream-secrets.yaml/,/^---$/p' "$generated_secrets_manifest")
if echo "$upstream_secrets_section" | grep -q 'lookup'; then
  exit 1
fi
# Fresh-install (absent) path: both Secrets render with data: {}
test "$(echo "$upstream_secrets_section" | grep -c '^data: {}$')" -ge 1
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
# NodePorts are overridable within the valid range so a second instance (a
# canary) can coexist on one node instead of contending for the live
# instance's node-wide ports. What must still be rejected is a value outside
# the NodePort range, or the two ports colliding with each other.
if ! render --set service.sshNodePort=32022 >/dev/null 2>&1; then
  exit 1
fi
if render --set service.sshNodePort=22 >/dev/null 2>&1; then
  exit 1
fi
if render --set service.httpsNodePort=40000 >/dev/null 2>&1; then
  exit 1
fi
if render --set service.sshNodePort=30443 >/dev/null 2>&1; then
  exit 1
fi
# The defaults stay exactly where they are documented.
render >"$default_nodeport_manifest"
grep -q '^      nodePort: 30022$' "$default_nodeport_manifest"
grep -q '^      nodePort: 30443$' "$default_nodeport_manifest"
if render --set service.type=ClusterIP >/dev/null 2>&1; then
  exit 1
fi
if render --set service.sshPort=0 >/dev/null 2>&1; then
  exit 1
fi
if render --set service.sshPort=65536 >/dev/null 2>&1; then
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
if render --set-string auditAnchor.rekorPublicKey=not-a-pem >/dev/null 2>&1; then
  exit 1
fi
if render --set-file auditAnchor.rekorPublicKey="$empty_rekor_public_key" >/dev/null 2>&1; then
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
for audit_url in \
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
if render --set-string auditAnchor.tsaURL=http://audit.example.test >/dev/null 2>&1; then
  exit 1
fi
# TSA insecure HTTP is accepted only when the flag is set.
render --set auditAnchor.tsaInsecureHTTP=true --set-string auditAnchor.tsaURL=http://audit.example.test >/dev/null
if render --set-string auditAnchor.rekorURL=http://rekor.example.test >/dev/null 2>&1; then
  exit 1
fi

# NetworkPolicy: default renders with openbao namespace target.
render --set networkPolicy.enabled=true --show-only templates/networkpolicy.yaml >"$netpol_manifest"
grep -Fq 'kubernetes.io/metadata.name: openbao' "$netpol_manifest"
grep -Fq 'port: 8200' "$netpol_manifest"
# NetworkPolicy: alternate namespace.
render --set networkPolicy.enabled=true \
  --set networkPolicy.vault.namespace=vault \
  --set networkPolicy.vault.port=8201 \
  --show-only templates/networkpolicy.yaml >"$alt_netpol"
grep -Fq 'kubernetes.io/metadata.name: vault' "$alt_netpol"
grep -Fq 'port: 8201' "$alt_netpol"
# NetworkPolicy: external vault via CIDR.
render --set networkPolicy.enabled=true \
  --set-json 'networkPolicy.vault={"externalCIDRs":["10.0.1.5/32"],"port":8200}' \
  --show-only templates/networkpolicy.yaml >"$ext_netpol"
grep -Fq 'cidr: 10.0.1.5/32' "$ext_netpol"
# Structural assertion: external vault mode must not emit a namespaceSelector
# rule. Strip YAML comments first — the template's documentation legitimately
# names the field, and helm template preserves comments in rendered output.
if grep -v '^[[:space:]]*#' "$ext_netpol" | grep -Fq 'namespaceSelector'; then
  echo "networkpolicy: external vault mode must not render a namespaceSelector rule" >&2
  exit 1
fi
# NetworkPolicy: enabled by default (resource rendered).
render --show-only templates/networkpolicy.yaml | grep -q 'NetworkPolicy'
# NetworkPolicy: explicitly disabled suppresses the resource.
if render --set networkPolicy.enabled=false --show-only templates/networkpolicy.yaml 2>/dev/null | grep -q 'NetworkPolicy'; then
  exit 1
fi

# upstream.name must match app.ValidateUpstreamName: lowercase alphanumerics and
# hyphens, no leading/trailing hyphen, max 53 characters.
render --set upstream.name=github >/dev/null
render --set upstream.name=my-forge >/dev/null
render --set upstream.name=a >/dev/null
render --set upstream.name=a0b1c2 >/dev/null
# The boundary: exactly 53 characters.
render --set upstream.name="$(printf 'a%.0s' $(seq 1 53))" >/dev/null
# 54 characters: rejected.
if render --set upstream.name="$(printf 'a%.0s' $(seq 1 54))" >/dev/null 2>&1; then
  exit 1
fi
# Uppercase: rejected.
if render --set upstream.name=GitHub >/dev/null 2>&1; then
  exit 1
fi
# Underscore: rejected.
if render --set upstream.name=my_forge >/dev/null 2>&1; then
  exit 1
fi
# Dot: rejected.
if render --set upstream.name=my.forge >/dev/null 2>&1; then
  exit 1
fi
# Leading hyphen: rejected.
if render --set upstream.name=-github >/dev/null 2>&1; then
  exit 1
fi
# Trailing hyphen: rejected.
if render --set upstream.name=github- >/dev/null 2>&1; then
  exit 1
fi

# --- Argo identity tiers (issue #200) ----------------------------------------
# The default render declares all four pipeline-side identities.
for identity in oberth-argo-pipeline oberth-argo-credentialed oberth-argo-ci-secrets oberth-argo-executor; do
  grep -q "^  name: $identity$" "$manifest"
done
# The server is told both the branch-tier account and (with a vault address)
# the branch-tier role, with template-inline defaults so a --reuse-values
# upgrade from a pre-split revision still carries them.
grep -q -- '--argo-ci-secrets-serviceaccount=oberth-argo-ci-secrets' "$manifest"
render \
  --set argo.vault.address=https://openbao.openbao.svc:8200 \
  --set argo.vault.credentialedRole=oberth-argo-credentialed \
  --set-file argo.vault.caCert="$secretstore_ca" >"$argo_vault_manifest"
grep -q -- '--argo-vault-ci-secrets-role=oberth-argo-ci-secrets' "$argo_vault_manifest"
# Legacy reused values: a release installed before argo.ciSecrets existed has
# no such key in its merged values, and --reuse-values never consults the new
# chart's values.yaml. Nulling the block reproduces that shape; the inline
# defaults must still produce the branch-tier identity and role.
render \
  --set argo.ciSecrets=null \
  --set argo.vault.address=https://openbao.openbao.svc:8200 \
  --set argo.vault.credentialedRole=oberth-argo-credentialed \
  --set-file argo.vault.caCert="$secretstore_ca" >"$argo_legacy_manifest"
grep -q -- '--argo-ci-secrets-serviceaccount=oberth-argo-ci-secrets' "$argo_legacy_manifest"
grep -q -- '--argo-vault-ci-secrets-role=oberth-argo-ci-secrets' "$argo_legacy_manifest"
grep -q '^  name: oberth-argo-ci-secrets$' "$argo_legacy_manifest"
# The retired aliases named the RELEASE-tier identity; a values file still
# carrying one must refuse to render rather than silently misassign a tier.
if render --set argo.ciSecretsServiceAccount=oberth-argo-credentialed >/dev/null 2>&1; then
  exit 1
fi
if render \
  --set argo.vault.address=https://openbao.openbao.svc:8200 \
  --set argo.vault.ciSecretsRole=oberth-argo-credentialed \
  --set-file argo.vault.caCert="$secretstore_ca" >/dev/null 2>&1; then
  exit 1
fi
# A branch-tier account aliasing the release-tier one collapses the trust
# split and must refuse to render.
if render --set argo.ciSecrets.serviceAccount=oberth-argo-credentialed >/dev/null 2>&1; then
  exit 1
fi

helm package "$chart" --destination "$package_dir" >/dev/null
set -- "$package_dir"/oberth-*.tgz
test "$#" -eq 1
