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

# Also gate upgrades from the actual previous released values shape (#1207).
"$(dirname -- "$0")/test-chart-upgrade.sh" "$chart"
"$(dirname -- "$0")/test-platform-protection.sh" "$chart"

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
clusterip_service_manifest=$(mktemp)
netpol_manifest=$(mktemp)
alt_netpol=$(mktemp)
ext_netpol=$(mktemp)
argo_vault_manifest=$(mktemp)
argo_legacy_manifest=$(mktemp)
package_dir=$(mktemp -d)
trap 'rm -f "$manifest" "$empty_release_manifest" "$generated_secrets_manifest" "$custom_anchor_manifest" "$custom_rekor_manifest" "$rotated_rekor_manifest" "$custom_rekor_deployment" "$rotated_rekor_deployment" "$rekor_public_key" "$rotated_rekor_public_key" "$empty_rekor_public_key" "$insecure_rekor_manifest" "$insecure_tsa_manifest" "$secure_secretstore_manifest" "$rotated_secretstore_manifest" "$dev_secretstore_manifest" "$staged_transit_manifest" "$secretstore_ca" "$rotated_secretstore_ca" "$default_service_manifest" "$default_nodeport_manifest" "$adoption_service_manifest" "$desired_service_manifest" "$clusterip_service_manifest" "$netpol_manifest" "$alt_netpol" "$ext_netpol" "$argo_vault_manifest" "$argo_legacy_manifest"; find "$package_dir" -delete' EXIT
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
render >"$generated_secrets_manifest"
render \
  --set auditAnchor.tsaURL=https://timestamp.sectigo.com/rfc3161 \
  --set auditAnchor.rekorURL=https://rekor.sigstore.dev \
  --set-file auditAnchor.tsaCA="$secretstore_ca" >"$custom_anchor_manifest"
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
sed -n '/# Source: oberth\/templates\/pvc.yaml/,/^---$/p' "$manifest" | grep -q '^    helm.sh/resource-policy: keep$'
# Server namespace access is limited to ConfigMaps. Exec belongs exclusively
# to the pipeline Role for source seeding.
if sed -n '/# Source: oberth\/templates\/rbac.yaml/,/^---$/p' "$manifest" | grep -Eq '"pods(/log|/exec)?"'; then
  echo 'server namespace Role grants unused pod access' >&2
  exit 1
fi
sed -n '/# Source: oberth\/templates\/rbac-argo.yaml/,/^---$/p' "$manifest" | grep -q 'resources: \["pods/exec"\]'
test "$(grep -c 'resources: \["pods/exec"\]' "$manifest")" -eq 1
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
grep -q -- '--ssh-host-key=/run/oberth-identities/server/ssh_host_key' "$manifest"
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
grep -Fq -- '--audit-tsa-ca=/etc/oberth/audit-tsa-ca/ca.crt' "$custom_anchor_manifest"
grep -q '^            name: oberth-audit-tsa-ca$' "$custom_anchor_manifest"
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
# Every identity comes from OpenBao; neither resources nor mounts use Secrets.
if grep -Eq 'kind: Secret$|secretKeyRef:|secretName:|resources: \["secrets"\]' "$manifest"; then
  exit 1
fi
if grep -q 'name: OBERTH_IDENTITY_STORE' "$manifest"; then exit 1; fi
grep -q 'medium: Memory' "$manifest"
for legacy in tls.existingSecret ssh.hostKey.existingSecret auditAnchor.rootsSecret; do
  if render --set "$legacy=old-identity" >/dev/null 2>&1; then exit 1; fi
done
if render --set secretstore.enabled=false >/dev/null 2>&1; then exit 1; fi

if render --set replicaCount=2 >/dev/null 2>&1; then
  exit 1
fi
# The grace period covers the 55s worst-case drain (HTTP 15s, scheduler
# finalization 30s, API background shutdown 10s), also under --reuse-values
# where the new chart default is absent.
grep -q '^      terminationGracePeriodSeconds: 75$' "$manifest"
render --set terminationGracePeriodSeconds=null | grep -q '^      terminationGracePeriodSeconds: 75$'
render --set terminationGracePeriodSeconds=120 | grep -q '^      terminationGracePeriodSeconds: 120$'
if render --set terminationGracePeriodSeconds=30 >/dev/null 2>&1; then
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
render --show-only templates/service.yaml \
  --set service.type=ClusterIP >"$clusterip_service_manifest"
grep -q '^  type: ClusterIP$' "$clusterip_service_manifest"
grep -A3 '^    - name: ssh$' "$clusterip_service_manifest" | grep -q '^      targetPort: ssh$'
grep -A3 '^    - name: https$' "$clusterip_service_manifest" | grep -q '^      targetPort: https$'
if grep -Eq 'nodePort:|hostPort:|loadBalancerIP:|externalIPs:' "$clusterip_service_manifest"; then
  exit 1
fi
if render --set service.type=ExternalName >/dev/null 2>&1; then
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
grep -Fq 'kubernetes.io/metadata.name: "kube-system"' "$netpol_manifest"
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
# External vault mode removes only the store namespace rule. The DNS
# namespace restriction must remain in force independently.
grep -Fq 'kubernetes.io/metadata.name: "kube-system"' "$ext_netpol"
if grep -Fq 'kubernetes.io/metadata.name: openbao' "$ext_netpol"; then
  echo "networkpolicy: external vault mode must not retain the store namespace rule" >&2
  exit 1
fi
# NetworkPolicy: enabled by default (resource rendered).
render --show-only templates/networkpolicy.yaml | grep -q 'NetworkPolicy'
# Pipeline ingress isolates listeners by default; either opt-out suppresses it.
render --show-only templates/pipeline-ingress-networkpolicy.yaml | grep -q 'name: oberth-pipeline-ingress'
for disabled in networkPolicy.enabled=false networkPolicy.pipelineIngress.enabled=false; do
  if render --set "$disabled" --show-only templates/pipeline-ingress-networkpolicy.yaml 2>/dev/null | grep -q 'kind: NetworkPolicy'; then
    echo "pipeline ingress unexpectedly rendered with $disabled" >&2
    exit 1
  fi
done
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

# The native chart emits no admission arguments. An unknown null also errors.
render >"$manifest"
if grep -Eq -- '--admission-|--step-cpu-request-percent' "$manifest"; then
  echo 'retired admission flags rendered' >&2
  exit 1
fi
for removed in admission=null admission.cpuEnabled=true admission.cpuHeadroomPercent=20 admission.stepCPURequestPercent=25 \
  admission.pressure.enabled=false admission.pressure.hardCpu=50 admission.pressure.hardIo=40 \
  admission.pressure.deferSeconds=300 admission.diskFloorBytes=0 admission.stallTimeout=10m \
  admission.stepStartupAllowance.enabled=false; do
  if render --set "$removed" >"$manifest" 2>&1; then
    echo "removed admission value accepted: $removed" >&2
    exit 1
  fi
  grep -Fq 'admission' "$manifest"
done

render >"$manifest"

# Run limits live in Argo: the chart renders no limits ConfigMap,
# no runtime-capacity ConfigMap and no run-ceiling flag.
# releases.serialize renders the one flag only when set.
sync_manifest=$(mktemp)
render --set releases.serialize=true >"$sync_manifest"
grep -q -- '--serialize-releases=true' "$sync_manifest"
render >"$sync_manifest"
if grep -q 'oberth-sync-limits\|--serialize-releases\|max-concurrent-jobs\|runtime-capacity' "$sync_manifest"; then
  exit 1
fi
rm -f "$sync_manifest"
# Removed values are refused by the schema at any value, stock or not.
for removed in maxConcurrentJobs=0 argoSync.semaphores.kvm=1 admission.runtimeCapacity.enabled=true \
  admission.exclusiveClasses.kvm=1 admission.sizeWeights.XL=0 admission.maxHeadWait=20m \
  admission.declaredResources.ioRuns=0 admission.legacyResources.L.cpu_milli=1 gitBackend=cli gitcache.shadow=false \
  vm.kvm.enabled=false trustedTests.runtime=old argo.vmLauncherServiceAccount=old \
  argo.containerDiskPrefixes[0]=example argo.kubevirtCeiling.maxVMs=16 watchTunnel.enabled=true \
  upstream.keySecret=shared upstream.knownHostsSecret=shared upstream.createSecrets=false auditAnchor.rootsKey=roots.pem \
  secretCreatePolicy.enabled=false argo.enabled=true argo.ciServiceAccount=old argo.ciSecretsServiceAccount=old \
  argo.vault.ciSecretsRole=old admission.pressure.mode=observe argo.ciSecrets.serviceAccount= argo.ciSecrets.vaultRole=; do
  if render --set "$removed" >/dev/null 2>&1; then
    echo "removed value accepted: $removed" >&2
    exit 1
  fi
done

# Executor scratch keeps a safe default for reused values and accepts overrides.
grep -q -- '--argo-executor-scratch-size-limit=1Gi' "$manifest"
render --set argo.executorScratchSizeLimit=4Gi >"$argo_legacy_manifest"
grep -q -- '--argo-executor-scratch-size-limit=4Gi' "$argo_legacy_manifest"
render --set argo.executorScratchSizeLimit=null >"$argo_legacy_manifest"
grep -q -- '--argo-executor-scratch-size-limit=1Gi' "$argo_legacy_manifest"

# Ordinary nonroot capability must stay off unless the exact built-in profile
# is selected. Selection grants GET (only) on the pipeline namespace's public
# ConfigMaps, so the server can follow the profile the controller runs.
if grep -q -- '--argo-controller-profile=' "$manifest"; then
  exit 1
fi
render --set argo.controllerProfile=nonroot-static-v1 >"$argo_legacy_manifest"
grep -q -- '--argo-controller-profile=nonroot-static-v1' "$argo_legacy_manifest"
render --set argo.controllerProfile=nonroot-static-v1 \
  --show-only templates/rbac-argo.yaml >"$argo_vault_manifest"
test "$(grep -c 'resources: \["configmaps"\]' "$argo_vault_manifest")" -eq 3
grep -A2 'resources: \["configmaps"\]' "$argo_vault_manifest" | \
  grep -q 'resourceNames: \["oberth-argo-nr-5f121f7f1f5ef468eb2208b1cf7e911746c8cb6173c1d24e"\]'
# Only GET is ever granted on ConfigMaps: no list, watch or write.
test "$(grep -A2 'resources: \["configmaps"\]' "$argo_vault_manifest" | grep -c 'verbs:')" -eq 3
test "$(grep -A2 'resources: \["configmaps"\]' "$argo_vault_manifest" | grep -c 'verbs: \["get"\]')" -eq 3
grep -q 'name: oberth-argo-controller-profile$' "$argo_vault_manifest"
if render --set argo.controllerProfile=unchecked >/dev/null 2>&1; then
  exit 1
fi
# Without a pin the server follows the controller's profile: no flag. An
# explicit pin (#1517) renders the verification flag; the frozen Role keeps
# its v0.17.9 rules either way (issue #813).
if grep -q -- '--argo-controller-profile-configmap' "$argo_legacy_manifest"; then exit 1; fi
render --set argo.controllerProfile=nonroot-static-v1 --set argo.controllerProfileConfigMap=oberth-argo-nr-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa >"$argo_legacy_manifest"
grep -q -- '--argo-controller-profile-configmap=oberth-argo-nr-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' "$argo_legacy_manifest"
grep -A2 'resources: \["configmaps"\]' "$argo_legacy_manifest" | grep -q 'resourceNames: \["oberth-argo-nr-5f121f7f1f5ef468eb2208b1cf7e911746c8cb6173c1d24e"\]'
if render --set argo.controllerProfile=nonroot-static-v1 --set argo.controllerProfileConfigMap=other >/dev/null 2>&1; then
  exit 1
fi

# argojob's fail-closed per-repo ServiceAccount verification (issue #467):
# the server Role in the pipeline namespace must grant read-only get on
# serviceaccounts (the plain resource, distinct from the serviceaccounts/token
# subresource above it) or every credentialed CI and release run dies at Job
# creation with a Forbidden error. Exactly one such rule, get-only.
render --show-only templates/rbac-argo.yaml >"$argo_vault_manifest"
test "$(grep -c 'resources: \["serviceaccounts"\]' "$argo_vault_manifest")" -eq 1
grep -A1 'resources: \["serviceaccounts"\]' "$argo_vault_manifest" | \
  grep -q 'verbs: \["get"\]'


helm package "$chart" --destination "$package_dir" >/dev/null
set -- "$package_dir"/oberth-*.tgz
test "$#" -eq 1

# An upgrade must explicitly preserve the original protocol identifiers. Missing
# reused values fail closed instead of selecting a new audit/witness identity.
for key in compatibility compatibility.schemaIdentity compatibility.auditDomain compatibility.witnessKeyInfo; do
  if render --set "$key=null" > /dev/null 2>&1; then
    echo "missing protocol compatibility value accepted: $key" >&2
    exit 1
  fi
done
render --set-string compatibility.schemaIdentity=example-schema-v1 \
  --set-string compatibility.auditDomain=example-audit-v1 \
  --set-string compatibility.witnessKeyInfo=example-audit-witness-v1 >"$manifest"
grep -A1 'name: OBERTH_SCHEMA_IDENTITY' "$manifest" | grep -Fq 'example-schema-v1'
grep -A1 'name: OBERTH_AUDIT_DOMAIN' "$manifest" | grep -Fq 'example-audit-v1'
grep -A1 'name: OBERTH_WITNESS_KEY_INFO' "$manifest" | grep -Fq 'example-audit-witness-v1'

# Namespace ownership is explicit; enabling a mirror without the complete
# administrator mapping is refused instead of intercepting unrelated modules.
if render --set argo.goProxy.enabled=true >/dev/null 2>&1; then exit 1; fi
render --set argo.goProxy.enabled=true \
  --set argo.goProxy.modulePrefix=go.example.test \
  --set argo.goProxy.repositoryPrefix=sample- \
  --set argo.goProxy.upstream=forge \
  --set argo.goProxy.organization=example >"$manifest"
for arg in module-prefix=go.example.test repository-prefix=sample- upstream=forge organization=example; do
  grep -Fq -- "--argo-goproxy-$arg" "$manifest"
done
render >"$manifest"
if grep -q -- '--argo-goproxy-listen=' "$manifest"; then exit 1; fi

# The read-only mirror survives the parent pod's dependency readiness flaps;
# API/SSH routing must retain the ordinary readiness gate (#1204).
render --set argo.goProxy.enabled=true \
  --set argo.goProxy.modulePrefix=go.example.test \
  --set argo.goProxy.upstream=forge \
  --set argo.goProxy.organization=example \
  --show-only templates/goproxy-service.yaml >"$manifest"
grep -Fq 'publishNotReadyAddresses: true' "$manifest"
render --show-only templates/service.yaml >"$manifest"
if grep -Fq 'publishNotReadyAddresses: true' "$manifest"; then
  echo "public API/SSH Service must retain readiness gating" >&2
  exit 1
fi
render --show-only templates/deployment.yaml >"$manifest"
for probe in readiness startup; do
  sed -n "/${probe}Probe:/,/periodSeconds:/p" "$manifest" | grep -Fq 'timeoutSeconds: 5'
done
# Liveness tolerates slow responses under load before counting a failure.
sed -n "/livenessProbe:/,/periodSeconds:/p" "$manifest" | grep -Fq 'timeoutSeconds: 15'

# --- Promotion grant max lifetime (#826) -------------------------------------
# Default renders the 24h lifetime flag.
promotion_manifest=$(mktemp)
render >"$promotion_manifest"
grep -Fq -- '--promotion-grant-max-lifetime=24h' "$promotion_manifest"

# Custom valid value renders correctly.
render --set promotion.grantMaxLifetime=48h >"$promotion_manifest"
grep -Fq -- '--promotion-grant-max-lifetime=48h' "$promotion_manifest"

# Over-ceiling value (>168h) must fail.
if render --set promotion.grantMaxLifetime=200h >/dev/null 2>&1; then exit 1; fi

# Invalid duration string must fail.
if render --set promotion.grantMaxLifetime=notaduration >/dev/null 2>&1; then exit 1; fi

# Legacy reused values: a release installed before promotion existed has no
# promotion key in its merged values, and --reuse-values never consults the
# new chart's values.yaml. Nulling the block reproduces that shape; the
# template must render without error and default to 24h.
render --set promotion=null >"$promotion_manifest"
grep -Fq -- '--promotion-grant-max-lifetime=24h' "$promotion_manifest"
rm -f "$promotion_manifest"



# Retention defaults keep logs by age, bound them to a 2 GiB budget and keep
# the existing artifact budget; no disk-space admission policy is rendered.
render >"$manifest"
for flag in --log-retention=0s --artifact-retention=0s --logs-budget-bytes=2147483648 --run-log-limit-bytes=67108864 \
  --artifacts-budget-bytes=4294967296 --artifacts-limit-bytes=268435456 \
  --retention-sweep-interval=10m --retention-keep-newest=1; do
  grep -Fq -- "$flag" "$manifest"
done
render --set retention.logs.maxAge=720h --set retention.logs.budgetBytes=123456 --set retention.logs.limitBytes=1234 \
  --set retention.artifacts.maxAge=168h --set retention.artifacts.budgetBytes=0 \
  --set retention.artifacts.limitBytes=654321 --set retention.interval=5m \
  --set retention.keepNewestPerRef=0 >"$manifest"
for flag in --log-retention=720h --artifact-retention=168h --logs-budget-bytes=123456 --run-log-limit-bytes=1234 \
  --artifacts-budget-bytes=0 --artifacts-limit-bytes=654321 \
  --retention-sweep-interval=5m --retention-keep-newest=0; do
  grep -Fq -- "$flag" "$manifest"
done
for value in retention.logs.limitBytes=0 retention.logs.limitBytes=-1 retention.logs.budgetBytes=-1 retention.artifacts.budgetBytes=-1 \
  retention.artifacts.limitBytes=-1 retention.keepNewestPerRef=-1; do
  if render --set "$value" >/dev/null 2>&1; then
    echo "chart accepted negative retention setting: $value" >&2
    exit 1
  fi
done
# Saved values from an older release have no retention object at all.
old_retention_chart=$package_dir/old-retention-chart
mkdir "$old_retention_chart"
cp -R -- "$chart"/. "$old_retention_chart"
chmod -R u+w -- "$old_retention_chart"
awk '/^retention:/ { skip=1; next } skip && /^[^ #]/ { skip=0 } !skip { print }' \
  "$chart/values.yaml" >"$old_retention_chart/values.yaml"
helm template oberth "$old_retention_chart" --set "image.ref=$server_image" >"$manifest"
for flag in --log-retention=0s --artifact-retention=0s --logs-budget-bytes=0 --run-log-limit-bytes=67108864 \
  --artifacts-budget-bytes=4294967296 --artifacts-limit-bytes=268435456 \
  --retention-sweep-interval=10m --retention-keep-newest=1; do
  grep -Fq -- "$flag" "$manifest"
done
# Archived repositories retain evidence but must have no release WIF grants.
archive_wif='{"namespace":"oberth-argo","roles":{"image-writer":{"provider":"projects/123/locations/global/workloadIdentityPools/images/providers/issuer","service_account":"publisher@example-project.iam.gserviceaccount.com"}},"repositories":{"github/org/retired":{"service_account_name":"retired-release","templates":{"publish":"image-writer"}}}}'
render --set-json "argo.releaseWIF=$archive_wif" >"$manifest"
if render --set-json 'archivedRepositories=["github/org/retired"]' --set-json "argo.releaseWIF=$archive_wif" >"$manifest" 2>&1; then
  echo "archived repository retained a releaseWIF capability" >&2
  exit 1
fi
grep -Fq 'releaseWIF entry for archived repository github/org/retired must be removed' "$manifest"
render --set-json 'archivedRepositories=["github/org/other"]' --set-json "argo.releaseWIF=$archive_wif" >"$manifest"
render --set-json 'archivedRepositories=["github/org/retired"]' >"$manifest"
render --set archivedRepositories=null >"$manifest"

# A reused pre-limit values file may have neither default. Validate partial
# objects against the schema without Helm merging today's sibling defaults.
partial_client_chart=$package_dir/partial-client-chart
mkdir "$partial_client_chart"
cp -R -- "$chart"/. "$partial_client_chart"
chmod -R u+w -- "$partial_client_chart"
awk '/^kubernetesClient:/ { skip=1; next } skip && /^[^ #]/ { skip=0 } !skip { print }' \
  "$chart/values.yaml" >"$partial_client_chart/values.yaml"
helm template oberth "$partial_client_chart" --set "image.ref=$server_image" \
  --set kubernetesClient.qps=75 >"$manifest"
grep -Fq -- '--kubernetes-client-qps=75' "$manifest"
grep -Fq -- '--kubernetes-client-burst=100' "$manifest"
helm template oberth "$partial_client_chart" --set "image.ref=$server_image" \
  --set kubernetesClient.burst=125 >"$manifest"
grep -Fq -- '--kubernetes-client-qps=50' "$manifest"
grep -Fq -- '--kubernetes-client-burst=125' "$manifest"
for invalid in kubernetesClient.qps=0 kubernetesClient.qps=-1 kubernetesClient.burst=0 kubernetesClient.burst=1.5 kubernetesClient.unknown=1; do
  if helm template oberth "$partial_client_chart" --set "image.ref=$server_image" \
    --set "$invalid" >"$manifest" 2>&1; then
    echo "invalid Kubernetes client limit accepted: $invalid" >&2
    exit 1
  fi
done
