#!/usr/bin/env bash
# Contract test for scripts/setup-secretstore.sh against mock bao/kubectl CLIs.
# Pins the script's security contract: idempotent re-runs mutate nothing,
# drifted objects are refused without --force, a config naming another cluster
# is refused without --force-auth-config, type/version mismatches fail closed,
# --dry-run performs no mutation, plain-HTTP and loopback addresses warn, and
# the in-cluster branch works without kubectl. Run by TestSetupSecretStore
# ScriptContract (embed_test.go) in every `go test ./...`.
#
# shellcheck disable=SC2016  # check() arguments are eval'd later by design.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
script=$root/scripts/setup-secretstore.sh

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
bin=$work/bin
mkdir -p "$bin"

# --- mock CLIs ---------------------------------------------------------------
cat >"$bin/bao" <<'MOCK'
#!/usr/bin/env bash
set -eu
state="$MOCK_STATE"
printf 'bao %s\n' "$*" >>"$state/calls.log"
cmd="${1:-}"; shift || true
case "$cmd" in
  token) exit 0 ;;
  auth)
    if [ "${1:-}" = "enable" ]; then printf 'kubernetes\n' >"$state/auth-type"; fi
    exit 0 ;;
  secrets)
    if [ "${1:-}" = "enable" ]; then printf 'kv\n' >"$state/kv-type"; printf '2\n' >"$state/kv-version"; fi
    exit 0 ;;
  read)
    field=""; path=""
    for argument in "$@"; do
      case "$argument" in -field=*) field="${argument#-field=}" ;; *) path="$argument" ;; esac
    done
    case "$field:$path" in
      type:sys/auth/*) [ -f "$state/auth-type" ] && cat "$state/auth-type" || exit 1 ;;
      kubernetes_host:auth/*/config) [ -f "$state/config-host" ] && cat "$state/config-host" || exit 1 ;;
      type:sys/mounts/*) [ -f "$state/kv-type" ] && cat "$state/kv-type" || exit 1 ;;
      options:sys/mounts/*)
        [ -f "$state/kv-version" ] || exit 1
        printf 'map[version:%s]\n' "$(cat "$state/kv-version")" ;;
      bound_service_account_names:auth/*/role/*) [ -f "$state/role-names" ] && cat "$state/role-names" || exit 1 ;;
      bound_service_account_namespaces:auth/*/role/*) [ -f "$state/role-namespaces" ] && cat "$state/role-namespaces" || exit 1 ;;
      token_policies:auth/*/role/*) [ -f "$state/role-policies" ] && cat "$state/role-policies" || exit 1 ;;
      *) exit 1 ;;
    esac ;;
  write)
    target="${1:-}"; shift || true
    case "$target" in
      auth/*/config)
        for argument in "$@"; do
          case "$argument" in kubernetes_host=*) printf '%s\n' "${argument#kubernetes_host=}" >"$state/config-host" ;; esac
        done ;;
      auth/*/role/*)
        for argument in "$@"; do
          case "$argument" in
            bound_service_account_names=*) printf '[%s]\n' "${argument#*=}" >"$state/role-names" ;;
            bound_service_account_namespaces=*) printf '[%s]\n' "${argument#*=}" >"$state/role-namespaces" ;;
            token_policies=*) printf '[%s]\n' "${argument#*=}" >"$state/role-policies" ;;
          esac
        done ;;
    esac
    exit 0 ;;
  policy)
    sub="${1:-}"; name="${2:-}"
    if [ "$sub" = "read" ]; then
      [ -f "$state/policy-$name" ] && cat "$state/policy-$name" || exit 1
    elif [ "$sub" = "write" ]; then
      cat >"$state/policy-$name"
    fi
    exit 0 ;;
  *) exit 0 ;;
esac
MOCK
cat >"$bin/kubectl" <<'MOCK'
#!/usr/bin/env bash
set -eu
printf 'kubectl %s\n' "$*" >>"$MOCK_STATE/calls.log"
case "$*" in
  "config current-context") printf 'test-context\n' ;;
  *".clusters[0].cluster.server"*) printf 'https://kube.example:6443\n' ;;
  *kube-root-ca.crt*) printf -- '-----BEGIN CERTIFICATE-----\nMIIFAKETESTCA\n-----END CERTIFICATE-----\n' ;;
  *) exit 1 ;;
esac
MOCK
chmod +x "$bin/bao" "$bin/kubectl"

export PATH="$bin:$PATH"
unset VAULT_ADDR VAULT_SKIP_VERIFY BAO_SKIP_VERIFY KUBERNETES_SERVICE_HOST IN_CLUSTER_CA 2>/dev/null || true
export BAO_ADDR="https://store.example:8200"

failures=0
scenario=""
fresh_state() {
  MOCK_STATE=$(mktemp -d "$work/state.XXXXXX")
  export MOCK_STATE
  : >"$MOCK_STATE/calls.log"
}
begin() { scenario="$1"; }
run_script() {
  status=0
  output=$("$script" "$@" 2>&1) || status=$?
}
check() {
  if ! eval "$1"; then
    failures=$((failures + 1))
    printf 'FAIL [%s]: %s\noutput:\n%s\n---\n' "$scenario" "$2" "$output" >&2
  fi
}
contains() { case "$output" in *"$1"*) return 0 ;; *) return 1 ;; esac }
calls_contain() { grep -Fq -- "$1" "$MOCK_STATE/calls.log"; }

# --- 1. fresh setup creates everything and prints the exact helm handoff -----
begin "fresh"
fresh_state
run_script
check '[ "$status" = 0 ]' "fresh run must succeed (status=$status)"
check 'contains "enabling Kubernetes auth at kubernetes/"' "must enable the auth method"
check 'contains "writing auth/kubernetes/config"' "must write the auth config"
check 'contains "enabling KV v2 secrets mount at oberth/"' "must enable the KV v2 mount"
check 'contains "writing read-only policy oberth-ci"' "must write the policy"
check 'contains "writing role oberth-ci bound to ServiceAccount oberth/oberth"' "must write the role"
check 'contains "secretstore.enabled=true"' "helm handoff must enable the secret store"
check 'contains "secretstore.address=https://store.example:8200"' "helm handoff must carry the address"
check 'contains "oberth secretstore verify"' "next steps must include the in-pod verification"
check 'calls_contain "bao auth enable -path=kubernetes kubernetes"' "auth enable must reach the CLI"
check 'calls_contain "token_no_default_policy=true"' "role must drop the default policy"
check 'grep -Fq "auth/token/revoke-self" "$MOCK_STATE/policy-oberth-ci"' "policy must allow the client to revoke its own login token"
check 'grep -Fq "https://kube.example:6443" "$MOCK_STATE/config-host"' "config must point at the kubeconfig API server"

# --- 2. immediate re-run is a no-op ------------------------------------------
begin "idempotent-rerun"
: >"$MOCK_STATE/calls.log"
run_script
check '[ "$status" = 0 ]' "re-run must succeed (status=$status)"
check 'contains "auth method already enabled at kubernetes/"' "auth method must be kept"
check 'contains "already points at https://kube.example:6443 — unchanged"' "config must be reported unchanged"
check 'contains "already exists (KV v2)"' "KV mount must be kept"
check 'contains "policy oberth-ci already matches — unchanged"' "policy must be reported unchanged"
check 'contains "role oberth-ci already binds ServiceAccount oberth/oberth"' "role must be reported unchanged"
check '! calls_contain "bao auth enable"' "re-run must not re-enable auth"
check '! calls_contain "bao secrets enable"' "re-run must not re-enable the mount"
check '! calls_contain "bao write"' "re-run must not write anything"
check '! calls_contain "bao policy write"' "re-run must not rewrite the policy"

# --- 3. drifted role is refused without --force, replaced with it ------------
begin "role-drift"
printf '[other-sa]\n' >"$MOCK_STATE/role-names"
run_script
check '[ "$status" != 0 ]' "drifted role must fail without --force"
check 'contains "role oberth-ci exists with a different binding"' "drift must name the role"
check 'contains "re-run with --force"' "drift message must point at --force"
run_script --force
check '[ "$status" = 0 ]' "--force must replace the drifted role (status=$status)"
check 'grep -Fq "[oberth]" "$MOCK_STATE/role-names"' "--force must rebind the ServiceAccount"

# --- 4. customized policy is refused without --force -------------------------
begin "policy-drift"
printf 'path "elsewhere/*" { capabilities = ["read"] }\n' >"$MOCK_STATE/policy-oberth-ci"
run_script
check '[ "$status" != 0 ]' "drifted policy must fail without --force"
check 'contains "policy oberth-ci exists with different content"' "drift must name the policy"
run_script --force
check '[ "$status" = 0 ]' "--force must restore the managed policy (status=$status)"
check 'grep -Fq "auth/token/revoke-self" "$MOCK_STATE/policy-oberth-ci"' "--force must restore the managed content"

# --- 5. auth mount of another type fails closed ------------------------------
begin "auth-type-mismatch"
fresh_state
printf 'jwt\n' >"$MOCK_STATE/auth-type"
run_script
check '[ "$status" != 0 ]' "foreign auth mount type must fail"
check 'contains "type '\''jwt'\'', not '\''kubernetes'\''"' "failure must name the conflicting type"

# --- 6. KV v1 mount fails closed ---------------------------------------------
begin "kv-v1-mismatch"
fresh_state
printf 'kv\n' >"$MOCK_STATE/kv-type"
printf '1\n' >"$MOCK_STATE/kv-version"
run_script
check '[ "$status" != 0 ]' "KV v1 mount must fail"
check 'contains "KV v1"' "failure must name the version conflict"

# --- 7. config naming another cluster is refused without --force-auth-config -
begin "cross-cluster-config"
fresh_state
printf 'kubernetes\n' >"$MOCK_STATE/auth-type"
printf 'https://other-cluster.example:6443\n' >"$MOCK_STATE/config-host"
run_script
check '[ "$status" != 0 ]' "config for another cluster must fail"
check 'contains "DIFFERENT cluster"' "failure must explain the cross-cluster risk"
check 'contains "one auth mount serves exactly one cluster"' "failure must state the invariant"
run_script --force-auth-config
check '[ "$status" = 0 ]' "--force-auth-config must repoint deliberately (status=$status)"
check 'grep -Fq "https://kube.example:6443" "$MOCK_STATE/config-host"' "config must be repointed"

# --- 8. plain HTTP warns, loopback rewrites the helm handoff -----------------
begin "http-warning"
fresh_state
BAO_ADDR="http://store.example:8200" run_script
check '[ "$status" = 0 ]' "plain HTTP must warn, not fail (status=$status)"
check 'contains "PLAIN HTTP"' "plain HTTP must be called out"

begin "loopback-address"
fresh_state
BAO_ADDR="https://127.0.0.1:8200" run_script
check '[ "$status" = 0 ]' "loopback address must still configure the store (status=$status)"
check 'contains "address-reachable-from-your-cluster"' "helm handoff must not embed the loopback address"

# --- 8b. --address overrides the environment and keeps posture checks --------
begin "address-flag"
fresh_state
BAO_ADDR="https://ambient.example:8200" run_script --address https://flag.example:8200
check '[ "$status" = 0 ]' "--address must be accepted (status=$status)"
check 'contains "secretstore.address=https://flag.example:8200"' "--address must override the ambient environment"

begin "address-flag-http"
fresh_state
run_script --address http://flag.example:8200
check '[ "$status" = 0 ]' "plain-HTTP --address must warn, not fail (status=$status)"
check 'contains "PLAIN HTTP"' "plain-HTTP --address must be subject to the posture warning"

# --- 9. dry-run performs no mutation -----------------------------------------
begin "dry-run"
fresh_state
run_script --dry-run
check '[ "$status" = 0 ]' "dry-run must succeed (status=$status)"
check 'contains "DRY-RUN"' "dry-run must print planned mutations"
check '[ ! -f "$MOCK_STATE/auth-type" ]' "dry-run must not enable the auth method"
check '[ ! -f "$MOCK_STATE/config-host" ]' "dry-run must not write the config"
check '[ ! -f "$MOCK_STATE/policy-oberth-ci" ]' "dry-run must not write the policy"
check '[ ! -f "$MOCK_STATE/role-names" ]' "dry-run must not write the role"
check 'contains "kubernetes_ca_cert=<cluster CA PEM>"' "dry-run must summarize the CA instead of dumping PEM"

# --- 10. in-cluster mode needs no kubectl ------------------------------------
begin "in-cluster"
fresh_state
ca_file="$work/in-cluster-ca.crt"
printf -- '-----BEGIN CERTIFICATE-----\nMIIINCLUSTERCA\n-----END CERTIFICATE-----\n' >"$ca_file"
restricted=$work/restricted-bin
mkdir -p "$restricted"
cp "$bin/bao" "$restricted/bao"
PATH="$restricted:/usr/bin:/bin" KUBERNETES_SERVICE_HOST=10.43.0.1 IN_CLUSTER_CA="$ca_file" run_script
check '[ "$status" = 0 ]' "in-cluster run must succeed without kubectl (status=$status)"
check 'contains "https://kubernetes.default.svc"' "in-cluster mode must use the in-cluster API endpoint"
check '! calls_contain "kubectl"' "in-cluster mode must not call kubectl"

if [ "$failures" != 0 ]; then
  printf '%d setup-secretstore contract check(s) failed\n' "$failures" >&2
  exit 1
fi
printf 'setup-secretstore contract: OK\n'
