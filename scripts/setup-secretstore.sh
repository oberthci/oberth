#!/usr/bin/env bash
# setup-secretstore.sh — configure OpenBao (or HashiCorp Vault) to trust Oberth.
#
# One command on the machine where your `bao` (or `vault`) CLI is already
# authenticated. It configures the SECRET STORE side only:
#
#   1. enables the Kubernetes auth method (if absent; verifies its type if not)
#   2. writes its config (kubernetes_host + cluster CA) — only if unset,
#      and refuses to repoint a config that names a different cluster
#   3. enables a KV v2 mount for Oberth secrets (if absent; verifies v2 if not)
#   4. writes a minimal read-only policy for that mount
#   5. creates a role bound to the Oberth ServiceAccount and namespace
#
# The CLUSTER side (ServiceAccount, TokenReview permission, server flags) is
# owned entirely by the Helm chart:
#
#   helm upgrade --install oberth oberth/oberth --namespace oberth \
#     --create-namespace --set secretstore.enabled=true ...
#
# Idempotent: re-running with the same inputs changes nothing and says so.
# Existing objects are never overwritten silently: a drifted policy or role
# needs --force, a config naming another cluster needs --force-auth-config.
# Nothing is persisted: the script uses your existing CLI session and never
# reads, writes, or prints a token. Secret values are never touched.
#
# Requirements: bao or vault CLI (authenticated). kubectl (read-only) when run
# outside the cluster; inside a pod the mounted ServiceAccount CA is used.

set -euo pipefail

MOUNT="kubernetes"
ROLE="oberth-ci"
POLICY="oberth-ci"
KV_PREFIX="oberth"
NAMESPACE="oberth"
SERVICE_ACCOUNT="oberth"
KUBERNETES_HOST=""
CLI=""
FORCE_AUTH_CONFIG="false"
FORCE="false"
DRY_RUN="false"
IN_CLUSTER_CA="${IN_CLUSTER_CA:-/var/run/secrets/kubernetes.io/serviceaccount/ca.crt}"

usage() {
  awk 'NR > 1 && !/^#/ { exit } NR > 1 { sub(/^# ?/, ""); print }' "$0"
  cat <<'USAGE'
Options:
  --mount NAME             Kubernetes auth mount path        (default: kubernetes)
  --role NAME              auth role for Oberth              (default: oberth-ci)
  --policy NAME            policy name                       (default: oberth-ci)
  --kv-prefix NAME         KV v2 mount for Oberth secrets    (default: oberth)
  --namespace NAME         Oberth Kubernetes namespace       (default: oberth)
  --service-account NAME   Oberth ServiceAccount name        (default: oberth)
  --kubernetes-host URL    API server URL the secret store uses for TokenReview
                           (default: in-cluster service URL when run in-cluster,
                            otherwise the current kubeconfig server)
  --cli bao|vault          force a specific CLI              (default: autodetect)
  --force                  overwrite a drifted policy or role
  --force-auth-config      overwrite an existing auth mount config
  --dry-run                print every mutation instead of executing it
  -h, --help               this help
USAGE
}

log()  { printf '  %s\n' "$*"; }
note() { printf '\033[1m» %s\033[0m\n' "$*"; }
warn() { printf '\033[33mWARNING: %s\033[0m\n' "$*" >&2; }
fail() { printf '\033[31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --mount) MOUNT="$2"; shift 2 ;;
    --role) ROLE="$2"; shift 2 ;;
    --policy) POLICY="$2"; shift 2 ;;
    --kv-prefix) KV_PREFIX="$2"; shift 2 ;;
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --service-account) SERVICE_ACCOUNT="$2"; shift 2 ;;
    --kubernetes-host) KUBERNETES_HOST="$2"; shift 2 ;;
    --cli) CLI="$2"; shift 2 ;;
    --force) FORCE="true"; shift ;;
    --force-auth-config) FORCE_AUTH_CONFIG="true"; shift ;;
    --dry-run) DRY_RUN="true"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown option: $1 (see --help)" ;;
  esac
done

case "$MOUNT$ROLE$POLICY$KV_PREFIX$NAMESPACE$SERVICE_ACCOUNT" in
  *[!A-Za-z0-9_.-]*) fail "names may use only letters, digits, dots, dashes, and underscores" ;;
esac

# --- CLI detection -----------------------------------------------------------
if [ -z "$CLI" ]; then
  if command -v bao >/dev/null 2>&1; then CLI="bao"
  elif command -v vault >/dev/null 2>&1; then CLI="vault"
  else fail "neither 'bao' nor 'vault' CLI found in PATH"
  fi
fi
command -v "$CLI" >/dev/null 2>&1 || fail "CLI '$CLI' not found in PATH"

STORE_ADDR="${BAO_ADDR:-${VAULT_ADDR:-}}"
[ -n "$STORE_ADDR" ] || fail "set BAO_ADDR or VAULT_ADDR to your secret store URL first"

# --- security posture checks -------------------------------------------------
case "$STORE_ADDR" in
  https://*) : ;;
  http://127.0.0.1*|http://localhost*|http://\[::1\]*)
    warn "secret store address $STORE_ADDR is loopback plain HTTP — acceptable only for local development" ;;
  http://*)
    warn "secret store address $STORE_ADDR uses PLAIN HTTP over the network."
    warn "Oberth requires https:// unless you explicitly set secretstore.insecureHTTPForDev." ;;
  *) fail "secret store address $STORE_ADDR must be an http(s) URL" ;;
esac
if [ -n "${BAO_SKIP_VERIFY:-}${VAULT_SKIP_VERIFY:-}" ]; then
  warn "TLS verification is DISABLED in your CLI environment (SKIP_VERIFY). Fix the trust chain instead; Oberth itself always verifies."
fi

"$CLI" token lookup >/dev/null 2>&1 || fail "$CLI is not authenticated against $STORE_ADDR; log in first ($CLI login)"

run() {
  if [ "$DRY_RUN" = "true" ]; then
    # Keep the dry-run readable: the cluster CA is public material but pastes
    # as a multi-kilobyte PEM block.
    printf '  DRY-RUN:'
    for word in "$@"; do
      case "$word" in
        kubernetes_ca_cert=*) printf ' %s' 'kubernetes_ca_cert=<cluster CA PEM>' ;;
        *) printf ' %s' "$word" ;;
      esac
    done
    printf '\n'
  else
    "$@" >/dev/null
  fi
}

# read_field prints one field of a store path, or nothing if the path or field
# is unreadable (older servers, missing object).
read_field() {
  "$CLI" read -field="$1" "$2" 2>/dev/null || true
}

# --- cluster facts (read-only) ----------------------------------------------
CLUSTER_CA=""
if [ -n "${KUBERNETES_SERVICE_HOST:-}" ] && [ -r "$IN_CLUSTER_CA" ]; then
  # Running inside the cluster (e.g. next to an in-cluster OpenBao): the store
  # reaches the API server through the in-cluster service address, and the
  # pod's mounted ServiceAccount bundle already carries the cluster CA —
  # kubectl is not needed at all.
  [ -n "$KUBERNETES_HOST" ] || KUBERNETES_HOST="https://kubernetes.default.svc"
  CLUSTER_CA="$(cat "$IN_CLUSTER_CA")"
else
  command -v kubectl >/dev/null 2>&1 || fail "kubectl not found in PATH (read-only use; needed to derive the API server URL and CA)"
  KUBE_CONTEXT="$(kubectl config current-context 2>/dev/null || true)"
  [ -n "$KUBE_CONTEXT" ] && note "using kubeconfig context: $KUBE_CONTEXT"
  if [ -z "$KUBERNETES_HOST" ]; then
    KUBERNETES_HOST="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
    [ -n "$KUBERNETES_HOST" ] || fail "could not derive the API server URL; pass --kubernetes-host"
  fi
  CLUSTER_CA="$(kubectl get configmap kube-root-ca.crt -n kube-public -o jsonpath='{.data.ca\.crt}' 2>/dev/null || true)"
  if [ -z "$CLUSTER_CA" ]; then
    CA_DATA="$(kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' 2>/dev/null || true)"
    if [ -n "$CA_DATA" ]; then
      CLUSTER_CA="$(printf '%s' "$CA_DATA" | base64 -d 2>/dev/null || printf '%s' "$CA_DATA" | base64 -D 2>/dev/null || true)"
    fi
  fi
fi
[ -n "$CLUSTER_CA" ] || fail "could not read the cluster CA (in-cluster bundle, kube-root-ca.crt, or kubeconfig); is kubectl pointed at the right cluster?"
note "TokenReview endpoint the secret store will call: $KUBERNETES_HOST"
log "the secret store must be able to REACH this URL; override with --kubernetes-host if not"

# --- 1. Kubernetes auth method ----------------------------------------------
AUTH_TYPE="$(read_field type "sys/auth/$MOUNT")"
if [ -z "$AUTH_TYPE" ]; then
  AUTH_TYPE="$("$CLI" auth list 2>/dev/null | awk -v mount="$MOUNT/" '$1 == mount { print $2 }')"
fi
if [ -n "$AUTH_TYPE" ]; then
  [ "$AUTH_TYPE" = "kubernetes" ] || fail "auth mount $MOUNT/ already exists with type '$AUTH_TYPE', not 'kubernetes'; pick a dedicated mount: --mount oberth-kubernetes"
  note "auth method already enabled at $MOUNT/ — keeping it"
else
  note "enabling Kubernetes auth at $MOUNT/"
  run "$CLI" auth enable -path="$MOUNT" kubernetes
fi

# --- 2. auth mount config (never silently overwritten or repointed) ----------
# No reviewer JWT is ever stored. An out-of-cluster store validates logins
# with the login JWT itself (the Helm chart's system:auth-delegator binding
# permits that TokenReview); an in-cluster store uses its own ServiceAccount.
EXISTING_HOST="$(read_field kubernetes_host "auth/$MOUNT/config")"
if [ -n "$EXISTING_HOST" ] && [ "$FORCE_AUTH_CONFIG" != "true" ]; then
  if [ "$EXISTING_HOST" = "$KUBERNETES_HOST" ]; then
    note "auth/$MOUNT/config already points at $KUBERNETES_HOST — unchanged"
  else
    warn "auth/$MOUNT/config points at a DIFFERENT cluster: $EXISTING_HOST"
    warn "one auth mount serves exactly one cluster; a role written here would authenticate ServiceAccounts of that other cluster."
    fail "use a dedicated mount per cluster (--mount oberth-<cluster>), or repoint this one deliberately with --force-auth-config"
  fi
else
  note "writing auth/$MOUNT/config (host + cluster CA; no reviewer token is stored)"
  run "$CLI" write "auth/$MOUNT/config" \
    kubernetes_host="$KUBERNETES_HOST" \
    kubernetes_ca_cert="$CLUSTER_CA"
fi

# --- 3. KV v2 mount for Oberth secrets ---------------------------------------
KV_TYPE="$(read_field type "sys/mounts/$KV_PREFIX")"
if [ -z "$KV_TYPE" ] && "$CLI" secrets list 2>/dev/null | awk -v mount="$KV_PREFIX/" '$1 == mount { found = 1 } END { exit !found }'; then
  KV_TYPE="$("$CLI" secrets list 2>/dev/null | awk -v mount="$KV_PREFIX/" '$1 == mount { print $2 }')"
fi
if [ -n "$KV_TYPE" ]; then
  case "$KV_TYPE" in
    kv|kv-v2|generic) : ;;
    *) fail "secrets mount $KV_PREFIX/ already exists with type '$KV_TYPE', not KV; pick another: --kv-prefix oberth-ci" ;;
  esac
  KV_OPTIONS="$(read_field options "sys/mounts/$KV_PREFIX")"
  case "$KV_OPTIONS" in
    *version:2*) note "secrets mount $KV_PREFIX/ already exists (KV v2) — keeping it" ;;
    "") note "secrets mount $KV_PREFIX/ already exists — keeping it"
        warn "could not determine its KV version; the policy below assumes v2 ($KV_PREFIX/data/*). Check: $CLI read sys/mounts/$KV_PREFIX" ;;
    *) fail "secrets mount $KV_PREFIX/ is KV v1; Oberth paths and the read-only policy assume KV v2. Upgrade it ($CLI kv enable-versioning $KV_PREFIX) or pick another: --kv-prefix oberth-ci" ;;
  esac
else
  note "enabling KV v2 secrets mount at $KV_PREFIX/"
  run "$CLI" secrets enable -path="$KV_PREFIX" -version=2 kv
fi

# --- 4. minimal read-only policy ---------------------------------------------
# revoke-self is the one extra grant: Oberth's client revokes its short-lived
# login token after every fetch instead of leaving it to expire, and the role
# below attaches no default policy that would otherwise permit that.
WANT_POLICY="$(cat <<POLICYEOF
# Oberth release secrets: read-only, data endpoints only. No list, no
# metadata, no write, no delete. Managed by setup-secretstore.sh.
path "$KV_PREFIX/data/*" {
  capabilities = ["read"]
}

# Allow the fetch client to revoke its own short-lived login token.
path "auth/token/revoke-self" {
  capabilities = ["update"]
}
POLICYEOF
)"
HAVE_POLICY="$("$CLI" policy read "$POLICY" 2>/dev/null || true)"
if [ -n "$HAVE_POLICY" ] && [ "$(printf '%s' "$HAVE_POLICY")" = "$(printf '%s' "$WANT_POLICY")" ]; then
  note "policy $POLICY already matches — unchanged"
elif [ -n "$HAVE_POLICY" ] && [ "$FORCE" != "true" ]; then
  warn "policy $POLICY exists with different content (it may have been customized):"
  printf '%s\n' "$HAVE_POLICY" | sed 's/^/    | /' >&2
  fail "re-run with --force to replace it with the managed read-only policy, or pass --policy <other-name>"
else
  note "writing read-only policy $POLICY (read on $KV_PREFIX/data/* only)"
  if [ "$DRY_RUN" = "true" ]; then
    log "DRY-RUN: $CLI policy write $POLICY <read-only policy for $KV_PREFIX/data/*>"
  else
    printf '%s\n' "$WANT_POLICY" | "$CLI" policy write "$POLICY" - >/dev/null
  fi
fi

# --- 5. role bound to the Oberth ServiceAccount ------------------------------
HAVE_ROLE_NAMES="$(read_field bound_service_account_names "auth/$MOUNT/role/$ROLE")"
if [ -n "$HAVE_ROLE_NAMES" ]; then
  HAVE_ROLE_NAMESPACES="$(read_field bound_service_account_namespaces "auth/$MOUNT/role/$ROLE")"
  HAVE_ROLE_POLICIES="$(read_field token_policies "auth/$MOUNT/role/$ROLE")"
  if [ "$HAVE_ROLE_NAMES" = "[$SERVICE_ACCOUNT]" ] && [ "$HAVE_ROLE_NAMESPACES" = "[$NAMESPACE]" ] && [ "$HAVE_ROLE_POLICIES" = "[$POLICY]" ]; then
    note "role $ROLE already binds ServiceAccount $NAMESPACE/$SERVICE_ACCOUNT with policy $POLICY — unchanged"
    WRITE_ROLE="false"
  elif [ "$FORCE" != "true" ]; then
    warn "role $ROLE exists with a different binding:"
    log "current: names=$HAVE_ROLE_NAMES namespaces=$HAVE_ROLE_NAMESPACES policies=$HAVE_ROLE_POLICIES"
    log "wanted:  names=[$SERVICE_ACCOUNT] namespaces=[$NAMESPACE] policies=[$POLICY]"
    fail "re-run with --force to replace it, or pass --role <other-name>"
  else
    WRITE_ROLE="true"
  fi
else
  WRITE_ROLE="true"
fi
if [ "$WRITE_ROLE" = "true" ]; then
  note "writing role $ROLE bound to ServiceAccount $NAMESPACE/$SERVICE_ACCOUNT"
  run "$CLI" write "auth/$MOUNT/role/$ROLE" \
    bound_service_account_names="$SERVICE_ACCOUNT" \
    bound_service_account_namespaces="$NAMESPACE" \
    token_policies="$POLICY" \
    token_no_default_policy=true \
    token_ttl=10m \
    token_max_ttl=15m
fi

# --- summary -----------------------------------------------------------------
HELM_ADDR="$STORE_ADDR"
case "$STORE_ADDR" in
  *//127.0.0.1*|*//localhost*|*//\[::1\]*)
    HELM_ADDR="<address-reachable-from-your-cluster>"
    warn "your store address is loopback ($STORE_ADDR): the cluster cannot reach it."
    warn "set secretstore.address to the URL your cluster resolves, e.g. https://openbao.example.eu:8200" ;;
esac

cat <<SUMMARY

Done. The secret store now trusts the Oberth ServiceAccount ($NAMESPACE/$SERVICE_ACCOUNT)
for short-lived, read-only access to $KV_PREFIX/data/*.

Next steps:

1. Put a release secret in the store (values must be strings):

     $CLI kv put $KV_PREFIX/r2-upload token=... account=...

2. Install or upgrade Oberth with the matching values:

     helm upgrade --install oberth oberth/oberth \\
       --namespace $NAMESPACE --create-namespace \\
       --set secretstore.enabled=true \\
       --set secretstore.address=$HELM_ADDR \\
       --set secretstore.k8sAuthMount=$MOUNT \\
       --set secretstore.role=$ROLE \\
       --set 'secretstore.allowedPaths={$KV_PREFIX/data/r2-upload}'

   (For a private CA, also set secretstore.caCert to the PEM bundle.)

3. Declare the secret in the repository's .oberth/periapsis.go:

     var SecretStoreSecrets = map[string]string{
             "r2-token": "$KV_PREFIX/data/r2-upload",
     }

   Release steps then read \$OBERTH_SECRETSTORE_DIR/r2-token/token. The values
   are fetched at release admission — a missing secret fails the run before
   its Job starts — and are delivered only into the release Job's memory.

4. Prove the whole trust chain end to end from inside the Oberth pod
   (ServiceAccount login, TokenReview, read policy, TLS — no credentials
   needed beyond the pod's own identity):

     kubectl exec -n $NAMESPACE deploy/oberth -- \\
       oberth secretstore verify $KV_PREFIX/data/r2-upload
SUMMARY
