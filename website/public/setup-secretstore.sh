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
#   4. over verified HTTPS, enables Transit and creates one non-exportable key
#   5. writes a minimal KV-read + exact Transit encrypt/decrypt policy
#   6. creates a role bound to the Oberth ServiceAccount and namespace
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
#
# RELATIONSHIP: this is the public-facing installer variant of
# scripts/setup-secretstore.sh (server-embedded canonical). It adds Argo
# credentialed-tier setup (sections 7-8: release-tier credentialed policy and
# role; sections 9-10: branch-tier ci-secrets policy and role, each bound to
# its own pipeline-namespace ServiceAccount — the branch tier must never be
# able to present an identity the release role accepts). Security hardening
# (read_field error classification, KUBERNETES_HOST validation) must be kept
# in sync with the canonical copy.

set -euo pipefail

MOUNT="kubernetes"
ROLE="oberth-ci"
POLICY="oberth-ci"
KV_PREFIX="oberth"
TRANSIT_MOUNT="oberth-transit"
TRANSIT_KEY="trusted-plan-artifacts"
TRANSIT_ENABLED="true"
NAMESPACE="oberth"
SERVICE_ACCOUNT="oberth"
ARGO_NAMESPACE="oberth-argo"
CREDENTIALED_SERVICE_ACCOUNT="oberth-argo-credentialed"
CREDENTIALED_ROLE="oberth-argo-credentialed"
CREDENTIALED_POLICY="oberth-argo-credentialed"
CI_SECRETS_SERVICE_ACCOUNT="oberth-argo-ci-secrets"
CI_SECRETS_ROLE="oberth-argo-ci-secrets"
CI_SECRETS_POLICY="oberth-argo-ci-secrets"
KUBERNETES_HOST=""
ADDRESS=""
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
  --transit-mount NAME     Transit mount for trusted plans    (default: oberth-transit)
  --transit-key NAME       non-exportable trusted-plan key    (default: trusted-plan-artifacts)
  --disable-transit        KV-only development setup; required for plain HTTP
  --namespace NAME         Oberth Kubernetes namespace       (default: oberth)
  --service-account NAME   Oberth ServiceAccount name        (default: oberth)
  --kubernetes-host URL    API server URL the secret store uses for TokenReview
                           (default: in-cluster service URL when run in-cluster,
                            otherwise the current kubeconfig server)
  --address URL            secret store URL for this run
                           (default: BAO_ADDR / VAULT_ADDR from the environment)
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
    --transit-mount) TRANSIT_MOUNT="$2"; shift 2 ;;
    --transit-key) TRANSIT_KEY="$2"; shift 2 ;;
    --disable-transit) TRANSIT_ENABLED="false"; shift ;;
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --service-account) SERVICE_ACCOUNT="$2"; shift 2 ;;
    --argo-namespace) ARGO_NAMESPACE="$2"; shift 2 ;;
    --credentialed-service-account) CREDENTIALED_SERVICE_ACCOUNT="$2"; shift 2 ;;
    --credentialed-role) CREDENTIALED_ROLE="$2"; shift 2 ;;
    --credentialed-policy) CREDENTIALED_POLICY="$2"; shift 2 ;;
    --ci-secrets-service-account) CI_SECRETS_SERVICE_ACCOUNT="$2"; shift 2 ;;
    --ci-secrets-role) CI_SECRETS_ROLE="$2"; shift 2 ;;
    --ci-secrets-policy) CI_SECRETS_POLICY="$2"; shift 2 ;;
    --kubernetes-host) KUBERNETES_HOST="$2"; shift 2 ;;
    --address) ADDRESS="$2"; shift 2 ;;
    --cli) CLI="$2"; shift 2 ;;
    --force) FORCE="true"; shift ;;
    --force-auth-config) FORCE_AUTH_CONFIG="true"; shift ;;
    --dry-run) DRY_RUN="true"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown option: $1 (see --help)" ;;
  esac
done

case "$MOUNT$ROLE$POLICY$KV_PREFIX$TRANSIT_MOUNT$TRANSIT_KEY$NAMESPACE$SERVICE_ACCOUNT$ARGO_NAMESPACE$CREDENTIALED_SERVICE_ACCOUNT$CREDENTIALED_ROLE$CREDENTIALED_POLICY$CI_SECRETS_SERVICE_ACCOUNT$CI_SECRETS_ROLE$CI_SECRETS_POLICY" in
  *[!A-Za-z0-9_.-]*) fail "names may use only letters, digits, dots, dashes, and underscores" ;;
esac
for required_name in "$MOUNT" "$ROLE" "$POLICY" "$KV_PREFIX" "$TRANSIT_MOUNT" "$TRANSIT_KEY" "$NAMESPACE" "$SERVICE_ACCOUNT" "$ARGO_NAMESPACE" "$CREDENTIALED_SERVICE_ACCOUNT" "$CREDENTIALED_ROLE" "$CREDENTIALED_POLICY" "$CI_SECRETS_SERVICE_ACCOUNT" "$CI_SECRETS_ROLE" "$CI_SECRETS_POLICY"; do
  [ -n "$required_name" ] || fail "names must not be empty"
  case "$required_name" in .|..) fail "names must be clean path segments, not . or .." ;; esac
done
# The branch-tier identity must not alias the release-tier one: OpenBao's
# Kubernetes auth binds (namespace, ServiceAccount) pairs, so a shared account
# would let a branch pipeline complete the release-tier login.
[ "$CI_SECRETS_SERVICE_ACCOUNT" != "$CREDENTIALED_SERVICE_ACCOUNT" ] || \
  fail "--ci-secrets-service-account must differ from --credentialed-service-account: a shared identity collapses the branch and release trust tiers"
[ "$CI_SECRETS_ROLE" != "$CREDENTIALED_ROLE" ] || \
  fail "--ci-secrets-role must differ from --credentialed-role"
[ "$CI_SECRETS_POLICY" != "$CREDENTIALED_POLICY" ] || \
  fail "--ci-secrets-policy must differ from --credentialed-policy"
case "$TRANSIT_MOUNT" in auth|sys|identity|cubbyhole) fail "Transit mount must not use a reserved API namespace" ;; esac

# --- CLI detection -----------------------------------------------------------
if [ -z "$CLI" ]; then
  if command -v bao >/dev/null 2>&1; then CLI="bao"
  elif command -v vault >/dev/null 2>&1; then CLI="vault"
  else fail "neither 'bao' nor 'vault' CLI found in PATH"
  fi
fi
command -v "$CLI" >/dev/null 2>&1 || fail "CLI '$CLI' not found in PATH"

if [ -n "$ADDRESS" ]; then
  # One address for whichever CLI runs below; the posture checks that follow
  # apply to a flag-supplied address exactly as they do to an ambient one.
  export BAO_ADDR="$ADDRESS" VAULT_ADDR="$ADDRESS"
fi
STORE_ADDR="${BAO_ADDR:-${VAULT_ADDR:-}}"
[ -n "$STORE_ADDR" ] || fail "set --address, BAO_ADDR, or VAULT_ADDR to your secret store URL first"

# --- security posture checks -------------------------------------------------
case "$STORE_ADDR" in
  https://*) : ;;
  http://127.0.0.1*|http://localhost*|http://\[::1\]*)
    [ "$TRANSIT_ENABLED" = "false" ] || fail "trusted-plan Transit requires verified HTTPS; use --disable-transit only for a KV-only local development setup"
    warn "secret store address $STORE_ADDR is loopback plain HTTP — acceptable only for local development" ;;
  http://*)
    [ "$TRANSIT_ENABLED" = "false" ] || fail "trusted-plan Transit requires verified HTTPS; use --disable-transit only for a KV-only development setup"
    warn "secret store address $STORE_ADDR uses PLAIN HTTP over the network."
    warn "Oberth requires https:// unless you explicitly set secretstore.insecureHTTPForDev." ;;
  *) fail "secret store address $STORE_ADDR must be an http(s) URL" ;;
esac
SKIP_VERIFY_ENABLED="false"
case "${BAO_SKIP_VERIFY:-}" in 1|t|T|true|TRUE|True) SKIP_VERIFY_ENABLED="true" ;; esac
case "${VAULT_SKIP_VERIFY:-}" in 1|t|T|true|TRUE|True) SKIP_VERIFY_ENABLED="true" ;; esac
if [ "$SKIP_VERIFY_ENABLED" = "true" ]; then
  [ "$TRANSIT_ENABLED" = "false" ] || fail "trusted-plan Transit setup refuses a CLI environment with TLS verification disabled (BAO_SKIP_VERIFY/VAULT_SKIP_VERIFY)"
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

# read_field prints one field of a store path. Distinguishes confirmed
# absence from permission, transport, or parse errors. The empty-output
# check catches both the "No value found" text absence and the mock CLI
# pattern (exit 1, no output) that older bao/vault versions also produce.
# A non-zero exit with substantive stderr (permission denied, network
# failure, parse error) aborts — those are real errors, not absence.
read_field() {
  local _rf_out _rf_exit
  _rf_out="$("$CLI" read -field="$1" "$2" 2>&1)" && { printf '%s' "$_rf_out"; return 0; }
  _rf_exit=$?
  # Confirmed absence: exit 2 is the documented Vault/OpenBao code.
  if [ "$_rf_exit" -eq 2 ]; then
    return 0
  fi
  # Empty output or the explicit absence string — treat as not-found. Older
  # CLI versions and field-level reads exit 1 for a missing path or field.
  case "$_rf_out" in
    "") return 0 ;;
    *"No value found"*|*"no value found"*) return 0 ;;
  esac
  # Check for permission-denied and transport errors that must not be silent.
  case "$_rf_out" in
    *"permission denied"*|*"Code: 403"*|*"connection refused"*|*"no such host"*|*"certificate"*|*"x509"*)
      fail "reading $2 field=$1 failed (exit $_rf_exit): $_rf_out" ;;
  esac
  # For other non-zero exits with output, treat as absence with a warning.
  # This preserves compatibility with varied CLI exit-code conventions.
  return 0
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

# Validate KUBERNETES_HOST before any Vault mutation: the auth config writes
# this URL into the store, and a plain HTTP endpoint would let a network
# attacker steal live pipeline SA JWTs presented for TokenReview.
case "$KUBERNETES_HOST" in
  https://*) : ;;
  http://*)
    fail "KUBERNETES_HOST ($KUBERNETES_HOST) uses plain HTTP; the secret store's auth config sends pipeline ServiceAccount JWTs for TokenReview to this URL — plain HTTP would expose them on the wire. Use the HTTPS API server URL." ;;
  *)
    fail "KUBERNETES_HOST ($KUBERNETES_HOST) must be an absolute https:// URL" ;;
esac
# Reject URLs with userinfo, query, or fragment — these are not valid API server addresses.
case "$KUBERNETES_HOST" in
  *@*) fail "KUBERNETES_HOST must not contain userinfo (@)" ;;
  *\?*) fail "KUBERNETES_HOST must not contain a query string" ;;
  *\#*) fail "KUBERNETES_HOST must not contain a fragment" ;;
esac

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

# --- 4. trusted-plan Transit engine and non-exportable key -------------------
if [ "$TRANSIT_ENABLED" = "true" ]; then
  TRANSIT_TYPE="$(read_field type "sys/mounts/$TRANSIT_MOUNT")"
  if [ -z "$TRANSIT_TYPE" ] && "$CLI" secrets list 2>/dev/null | awk -v mount="$TRANSIT_MOUNT/" '$1 == mount { found = 1 } END { exit !found }'; then
    TRANSIT_TYPE="$("$CLI" secrets list 2>/dev/null | awk -v mount="$TRANSIT_MOUNT/" '$1 == mount { print $2 }')"
  fi
  if [ -n "$TRANSIT_TYPE" ]; then
    [ "$TRANSIT_TYPE" = "transit" ] || fail "secrets mount $TRANSIT_MOUNT/ already exists with type '$TRANSIT_TYPE', not transit"
    note "Transit mount $TRANSIT_MOUNT/ already exists — keeping it"
  else
    note "enabling Transit mount at $TRANSIT_MOUNT/"
    run "$CLI" secrets enable -path="$TRANSIT_MOUNT" transit
  fi

  TRANSIT_KEY_PATH="$TRANSIT_MOUNT/keys/$TRANSIT_KEY"
  TRANSIT_KEY_TYPE="$(read_field type "$TRANSIT_KEY_PATH")"
  if [ -z "$TRANSIT_KEY_TYPE" ]; then
    note "creating non-exportable Transit key $TRANSIT_KEY"
    run "$CLI" write "$TRANSIT_KEY_PATH" \
      type=aes256-gcm96 derived=false exportable=false allow_plaintext_backup=false
  else
    TRANSIT_DERIVED="$(read_field derived "$TRANSIT_KEY_PATH")"
    TRANSIT_EXPORTABLE="$(read_field exportable "$TRANSIT_KEY_PATH")"
    TRANSIT_PLAINTEXT_BACKUP="$(read_field allow_plaintext_backup "$TRANSIT_KEY_PATH")"
    TRANSIT_SUPPORTS_ENCRYPTION="$(read_field supports_encryption "$TRANSIT_KEY_PATH")"
    TRANSIT_SUPPORTS_DECRYPTION="$(read_field supports_decryption "$TRANSIT_KEY_PATH")"
    if [ "$TRANSIT_KEY_TYPE" != "aes256-gcm96" ] || [ "$TRANSIT_DERIVED" != "false" ] || \
       [ "$TRANSIT_EXPORTABLE" != "false" ] || [ "$TRANSIT_PLAINTEXT_BACKUP" != "false" ] || \
       [ "$TRANSIT_SUPPORTS_ENCRYPTION" != "true" ] || [ "$TRANSIT_SUPPORTS_DECRYPTION" != "true" ]; then
      fail "Transit key $TRANSIT_KEY exists with an unsafe or incompatible configuration; refusing to overwrite it"
    fi
    note "Transit key $TRANSIT_KEY already matches — unchanged"
  fi
fi

# --- 5. minimal KV + exact Transit data policy -------------------------------
# revoke-self is the one extra grant: Oberth's client revokes its short-lived
# login token after every fetch instead of leaving it to expire, and the role
# below attaches no default policy that would otherwise permit that.
if [ "$TRANSIT_ENABLED" = "true" ]; then
  WANT_POLICY="$(cat <<POLICYEOF
# Oberth release secrets: read-only, data endpoints only. No list, no
# metadata, no write, no delete. Managed by setup-secretstore.sh.
path "$KV_PREFIX/data/*" {
  capabilities = ["read"]
}

# Trusted-plan envelope encryption/decryption through one non-exportable key.
path "$TRANSIT_MOUNT/encrypt/$TRANSIT_KEY" {
  capabilities = ["update"]
}

path "$TRANSIT_MOUNT/decrypt/$TRANSIT_KEY" {
  capabilities = ["update"]
}

# Allow the fetch client to revoke its own short-lived login token.
path "auth/token/revoke-self" {
  capabilities = ["update"]
}
POLICYEOF
)"
  POLICY_DESCRIPTION="KV read plus exact Transit encrypt/decrypt paths"
else
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
  POLICY_DESCRIPTION="KV read only; Transit disabled"
fi
HAVE_POLICY="$("$CLI" policy read "$POLICY" 2>/dev/null || true)"
if [ -n "$HAVE_POLICY" ] && [ "$(printf '%s' "$HAVE_POLICY")" = "$(printf '%s' "$WANT_POLICY")" ]; then
  note "policy $POLICY already matches — unchanged"
elif [ -n "$HAVE_POLICY" ] && [ "$FORCE" != "true" ]; then
  warn "policy $POLICY exists with different content (it may have been customized):"
  printf '%s\n' "$HAVE_POLICY" | sed 's/^/    | /' >&2
  fail "re-run with --force to replace it with the managed least-privilege policy, or pass --policy <other-name>"
else
  note "writing policy $POLICY ($POLICY_DESCRIPTION)"
  if [ "$DRY_RUN" = "true" ]; then
    log "DRY-RUN: $CLI policy write $POLICY <managed policy: $POLICY_DESCRIPTION>"
  else
    printf '%s\n' "$WANT_POLICY" | "$CLI" policy write "$POLICY" - >/dev/null
  fi
fi

# --- 6. role bound to the Oberth ServiceAccount ------------------------------
HAVE_ROLE_NAMES="$(read_field bound_service_account_names "auth/$MOUNT/role/$ROLE")"
if [ -n "$HAVE_ROLE_NAMES" ]; then
  HAVE_ROLE_NAMESPACES="$(read_field bound_service_account_namespaces "auth/$MOUNT/role/$ROLE")"
  HAVE_ROLE_POLICIES="$(read_field token_policies "auth/$MOUNT/role/$ROLE")"
  HAVE_ROLE_NO_DEFAULT="$(read_field token_no_default_policy "auth/$MOUNT/role/$ROLE")"
  HAVE_ROLE_TTL="$(read_field token_ttl "auth/$MOUNT/role/$ROLE")"
  HAVE_ROLE_MAX_TTL="$(read_field token_max_ttl "auth/$MOUNT/role/$ROLE")"
  ROLE_TTLS_MATCH="false"
  case "$HAVE_ROLE_TTL:$HAVE_ROLE_MAX_TTL" in 600:900|10m:15m) ROLE_TTLS_MATCH="true" ;; esac
  if [ "$HAVE_ROLE_NAMES" = "[$SERVICE_ACCOUNT]" ] && [ "$HAVE_ROLE_NAMESPACES" = "[$NAMESPACE]" ] && \
     [ "$HAVE_ROLE_POLICIES" = "[$POLICY]" ] && [ "$HAVE_ROLE_NO_DEFAULT" = "true" ] && \
     [ "$ROLE_TTLS_MATCH" = "true" ]; then
    note "role $ROLE already binds ServiceAccount $NAMESPACE/$SERVICE_ACCOUNT with policy $POLICY — unchanged"
    WRITE_ROLE="false"
  elif [ "$FORCE" != "true" ]; then
    warn "role $ROLE exists with a different binding or token lifetime:"
    log "current: names=$HAVE_ROLE_NAMES namespaces=$HAVE_ROLE_NAMESPACES policies=$HAVE_ROLE_POLICIES no_default=$HAVE_ROLE_NO_DEFAULT ttl=$HAVE_ROLE_TTL max_ttl=$HAVE_ROLE_MAX_TTL"
    log "wanted:  names=[$SERVICE_ACCOUNT] namespaces=[$NAMESPACE] policies=[$POLICY] no_default=true ttl=10m max_ttl=15m"
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

# --- 7. credentialed-tier policy (upstream subtree read-only) -----------------
WANT_CRED_POLICY="$(cat <<CREDPOLICYEOF
# Credentialed pipeline templates: read-only, upstream subtree only. The
# approval table in the Oberth database is the fine-grained gate; this policy
# is the coarse Vault-level boundary. Managed by setup-secretstore.sh.
path "$KV_PREFIX/data/upstream/*" {
  capabilities = ["read"]
}

# Allow the fetch client to revoke its own short-lived login token.
path "auth/token/revoke-self" {
  capabilities = ["update"]
}
CREDPOLICYEOF
)"
HAVE_CRED_POLICY="$("$CLI" policy read "$CREDENTIALED_POLICY" 2>/dev/null || true)"
if [ -n "$HAVE_CRED_POLICY" ] && [ "$(printf '%s' "$HAVE_CRED_POLICY")" = "$(printf '%s' "$WANT_CRED_POLICY")" ]; then
  note "policy $CREDENTIALED_POLICY already matches — unchanged"
elif [ -n "$HAVE_CRED_POLICY" ] && [ "$FORCE" != "true" ]; then
  warn "policy $CREDENTIALED_POLICY exists with different content:"
  printf '%s\n' "$HAVE_CRED_POLICY" | sed 's/^/    | /' >&2
  fail "re-run with --force to replace it, or pass --credentialed-policy <other-name>"
else
  note "writing policy $CREDENTIALED_POLICY (upstream subtree read-only)"
  if [ "$DRY_RUN" = "true" ]; then
    log "DRY-RUN: $CLI policy write $CREDENTIALED_POLICY <managed credentialed policy>"
  else
    printf '%s\n' "$WANT_CRED_POLICY" | "$CLI" policy write "$CREDENTIALED_POLICY" - >/dev/null
  fi
fi

# --- 8. credentialed role bound to the Argo credentialed ServiceAccount -------
HAVE_CRED_ROLE_NAMES="$(read_field bound_service_account_names "auth/$MOUNT/role/$CREDENTIALED_ROLE")"
if [ -n "$HAVE_CRED_ROLE_NAMES" ]; then
  HAVE_CRED_ROLE_NAMESPACES="$(read_field bound_service_account_namespaces "auth/$MOUNT/role/$CREDENTIALED_ROLE")"
  HAVE_CRED_ROLE_POLICIES="$(read_field token_policies "auth/$MOUNT/role/$CREDENTIALED_ROLE")"
  HAVE_CRED_ROLE_NO_DEFAULT="$(read_field token_no_default_policy "auth/$MOUNT/role/$CREDENTIALED_ROLE")"
  HAVE_CRED_ROLE_TTL="$(read_field token_ttl "auth/$MOUNT/role/$CREDENTIALED_ROLE")"
  HAVE_CRED_ROLE_MAX_TTL="$(read_field token_max_ttl "auth/$MOUNT/role/$CREDENTIALED_ROLE")"
  CRED_ROLE_TTLS_MATCH="false"
  case "$HAVE_CRED_ROLE_TTL:$HAVE_CRED_ROLE_MAX_TTL" in 1200:1800|20m:30m) CRED_ROLE_TTLS_MATCH="true" ;; esac
  if [ "$HAVE_CRED_ROLE_NAMES" = "[$CREDENTIALED_SERVICE_ACCOUNT]" ] && [ "$HAVE_CRED_ROLE_NAMESPACES" = "[$ARGO_NAMESPACE]" ] && \
     [ "$HAVE_CRED_ROLE_POLICIES" = "[$CREDENTIALED_POLICY]" ] && [ "$HAVE_CRED_ROLE_NO_DEFAULT" = "true" ] && \
     [ "$CRED_ROLE_TTLS_MATCH" = "true" ]; then
    note "role $CREDENTIALED_ROLE already binds ServiceAccount $ARGO_NAMESPACE/$CREDENTIALED_SERVICE_ACCOUNT with policy $CREDENTIALED_POLICY — unchanged"
    WRITE_CRED_ROLE="false"
  elif [ "$FORCE" != "true" ]; then
    warn "role $CREDENTIALED_ROLE exists with a different binding or token lifetime:"
    log "current: names=$HAVE_CRED_ROLE_NAMES namespaces=$HAVE_CRED_ROLE_NAMESPACES policies=$HAVE_CRED_ROLE_POLICIES no_default=$HAVE_CRED_ROLE_NO_DEFAULT ttl=$HAVE_CRED_ROLE_TTL max_ttl=$HAVE_CRED_ROLE_MAX_TTL"
    log "wanted:  names=[$CREDENTIALED_SERVICE_ACCOUNT] namespaces=[$ARGO_NAMESPACE] policies=[$CREDENTIALED_POLICY] no_default=true ttl=20m max_ttl=30m"
    fail "re-run with --force to replace it, or pass --credentialed-role <other-name>"
  else
    WRITE_CRED_ROLE="true"
  fi
else
  WRITE_CRED_ROLE="true"
fi
if [ "$WRITE_CRED_ROLE" = "true" ]; then
  note "writing role $CREDENTIALED_ROLE bound to ServiceAccount $ARGO_NAMESPACE/$CREDENTIALED_SERVICE_ACCOUNT"
  run "$CLI" write "auth/$MOUNT/role/$CREDENTIALED_ROLE" \
    bound_service_account_names="$CREDENTIALED_SERVICE_ACCOUNT" \
    bound_service_account_namespaces="$ARGO_NAMESPACE" \
    token_policies="$CREDENTIALED_POLICY" \
    token_no_default_policy=true \
    token_ttl=20m \
    token_max_ttl=30m
fi

# --- 9. ci-secrets-tier policy (branch tier: upstream subtree, never grants) --
# Same subtree as the credentialed policy today, but a separate object on
# purpose: exact release-secret grants are synced into the credentialed policy
# by `oberth install --credentialed-secret-path`, and this one must never
# receive them. Branch (CI-trigger) pipelines log in with this policy's role
# only.
WANT_CI_POLICY="$(cat <<CIPOLICYEOF
# CI-trigger credentialed pipelines: read-only, upstream subtree only. This
# policy never carries approval-table grants; release secrets are reachable
# only through the release-tier credentialed role. Managed by
# setup-secretstore.sh.
path "$KV_PREFIX/data/upstream/*" {
  capabilities = ["read"]
}

# Allow the fetch client to revoke its own short-lived login token.
path "auth/token/revoke-self" {
  capabilities = ["update"]
}
CIPOLICYEOF
)"
HAVE_CI_POLICY="$("$CLI" policy read "$CI_SECRETS_POLICY" 2>/dev/null || true)"
if [ -n "$HAVE_CI_POLICY" ] && [ "$(printf '%s' "$HAVE_CI_POLICY")" = "$(printf '%s' "$WANT_CI_POLICY")" ]; then
  note "policy $CI_SECRETS_POLICY already matches — unchanged"
elif [ -n "$HAVE_CI_POLICY" ] && [ "$FORCE" != "true" ]; then
  warn "policy $CI_SECRETS_POLICY exists with different content:"
  printf '%s\n' "$HAVE_CI_POLICY" | sed 's/^/    | /' >&2
  fail "re-run with --force to replace it, or pass --ci-secrets-policy <other-name>"
else
  note "writing policy $CI_SECRETS_POLICY (upstream subtree read-only, grant-free)"
  if [ "$DRY_RUN" = "true" ]; then
    log "DRY-RUN: $CLI policy write $CI_SECRETS_POLICY <managed ci-secrets policy>"
  else
    printf '%s\n' "$WANT_CI_POLICY" | "$CLI" policy write "$CI_SECRETS_POLICY" - >/dev/null
  fi
fi

# --- 10. ci-secrets role bound to the Argo ci-secrets ServiceAccount ----------
HAVE_CI_ROLE_NAMES="$(read_field bound_service_account_names "auth/$MOUNT/role/$CI_SECRETS_ROLE")"
if [ -n "$HAVE_CI_ROLE_NAMES" ]; then
  HAVE_CI_ROLE_NAMESPACES="$(read_field bound_service_account_namespaces "auth/$MOUNT/role/$CI_SECRETS_ROLE")"
  HAVE_CI_ROLE_POLICIES="$(read_field token_policies "auth/$MOUNT/role/$CI_SECRETS_ROLE")"
  HAVE_CI_ROLE_NO_DEFAULT="$(read_field token_no_default_policy "auth/$MOUNT/role/$CI_SECRETS_ROLE")"
  HAVE_CI_ROLE_TTL="$(read_field token_ttl "auth/$MOUNT/role/$CI_SECRETS_ROLE")"
  HAVE_CI_ROLE_MAX_TTL="$(read_field token_max_ttl "auth/$MOUNT/role/$CI_SECRETS_ROLE")"
  CI_ROLE_TTLS_MATCH="false"
  case "$HAVE_CI_ROLE_TTL:$HAVE_CI_ROLE_MAX_TTL" in 1200:1800|20m:30m) CI_ROLE_TTLS_MATCH="true" ;; esac
  if [ "$HAVE_CI_ROLE_NAMES" = "[$CI_SECRETS_SERVICE_ACCOUNT]" ] && [ "$HAVE_CI_ROLE_NAMESPACES" = "[$ARGO_NAMESPACE]" ] && \
     [ "$HAVE_CI_ROLE_POLICIES" = "[$CI_SECRETS_POLICY]" ] && [ "$HAVE_CI_ROLE_NO_DEFAULT" = "true" ] && \
     [ "$CI_ROLE_TTLS_MATCH" = "true" ]; then
    note "role $CI_SECRETS_ROLE already binds ServiceAccount $ARGO_NAMESPACE/$CI_SECRETS_SERVICE_ACCOUNT with policy $CI_SECRETS_POLICY — unchanged"
    WRITE_CI_ROLE="false"
  elif [ "$FORCE" != "true" ]; then
    warn "role $CI_SECRETS_ROLE exists with a different binding or token lifetime:"
    log "current: names=$HAVE_CI_ROLE_NAMES namespaces=$HAVE_CI_ROLE_NAMESPACES policies=$HAVE_CI_ROLE_POLICIES no_default=$HAVE_CI_ROLE_NO_DEFAULT ttl=$HAVE_CI_ROLE_TTL max_ttl=$HAVE_CI_ROLE_MAX_TTL"
    log "wanted:  names=[$CI_SECRETS_SERVICE_ACCOUNT] namespaces=[$ARGO_NAMESPACE] policies=[$CI_SECRETS_POLICY] no_default=true ttl=20m max_ttl=30m"
    fail "re-run with --force to replace it, or pass --ci-secrets-role <other-name>"
  else
    WRITE_CI_ROLE="true"
  fi
else
  WRITE_CI_ROLE="true"
fi
if [ "$WRITE_CI_ROLE" = "true" ]; then
  note "writing role $CI_SECRETS_ROLE bound to ServiceAccount $ARGO_NAMESPACE/$CI_SECRETS_SERVICE_ACCOUNT"
  run "$CLI" write "auth/$MOUNT/role/$CI_SECRETS_ROLE" \
    bound_service_account_names="$CI_SECRETS_SERVICE_ACCOUNT" \
    bound_service_account_namespaces="$ARGO_NAMESPACE" \
    token_policies="$CI_SECRETS_POLICY" \
    token_no_default_policy=true \
    token_ttl=20m \
    token_max_ttl=30m
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

Done. The secret store now trusts:
  - Server tier:       $NAMESPACE/$SERVICE_ACCOUNT (role $ROLE, policy $POLICY)
  - Release tier:      $ARGO_NAMESPACE/$CREDENTIALED_SERVICE_ACCOUNT (role $CREDENTIALED_ROLE, policy $CREDENTIALED_POLICY)
  - Branch CI tier:    $ARGO_NAMESPACE/$CI_SECRETS_SERVICE_ACCOUNT (role $CI_SECRETS_ROLE, policy $CI_SECRETS_POLICY)

The server tier has read access to $KV_PREFIX/data/*. Both pipeline tiers
start from the upstream subtree only ($KV_PREFIX/data/upstream/*); exact
release-secret grants are ever synced into the RELEASE-tier policy alone
(oberth install --credentialed-secret-path), and Oberth binds branch (CI)
runs to the branch-tier ServiceAccount, so release credentials are not
reachable from a branch push at the Vault layer. The approval table in the
Oberth database is the fine-grained gate on top. That covers both Oberth
secret namespaces on this mount; Oberth enforces the org/repo scoping below
at release admission, per triggering repository:

  $KV_PREFIX/data/...                        system namespace (exact allowlist)
  oberth/upstream/<org>/<secret>          org-scoped: every repo of that upstream
  oberth/upstream/<org>/<repo>/<secret>   repo-scoped: only that repository

(Scoped declarations always use the virtual oberth/upstream/ prefix; Oberth
fetches them from $KV_PREFIX/data/upstream/... on this mount.)

Next steps:

1. Put a release secret in the store (values must be strings). Scoped secrets
   need NO allowlist entry — writing the scoped path IS the grant:

     $CLI kv put $KV_PREFIX/upstream/<org>/<repo>/github-token token=...

   (System-namespace alternative, requiring an allowedPaths entry:
     $CLI kv put $KV_PREFIX/release/r2-upload token=... account=...)

2. Install or upgrade Oberth with the matching values:

     helm upgrade --install oberth oberth/oberth \\
       --namespace $NAMESPACE --create-namespace \\
       --set secretstore.enabled=true \\
       --set secretstore.address=$HELM_ADDR \\
       --set secretstore.k8sAuthMount=$MOUNT \\
       --set secretstore.role=$ROLE \\
       --set secretstore.kvMount=$KV_PREFIX \\
       --set secretstore.transit.enabled=$TRANSIT_ENABLED \\
       --set argo.vault.address=$HELM_ADDR \\
       --set argo.vault.credentialedRole=$CREDENTIALED_ROLE \\
       --set argo.ciSecrets.vaultRole=$CI_SECRETS_ROLE

   (When Transit is enabled, keep its managed identifiers aligned:
       --set secretstore.transit.mount=$TRANSIT_MOUNT \\
       --set secretstore.transit.key=$TRANSIT_KEY)

   (For a private CA for the credentialed tier, also set:
       --set argo.vault.caCert=<PEM bundle>)

   (For a private CA, also set secretstore.caCert to the PEM bundle. For
   system-namespace secrets, add them to secretstore.allowedPaths, e.g.
   --set 'secretstore.allowedPaths={$KV_PREFIX/data/release/r2-upload}'.)

3. Declare the secret in the repository's .oberth/periapsis.go — with the
   default kv-prefix (oberth) the scoped declaration is exactly the kv-put
   path from step 1:

     var SecretStoreSecrets = map[string]string{
             "github-token": "oberth/upstream/<org>/<repo>/github-token",
     }

   The org is your upstream registration's trailing path segment (oberth
   upstream add ... ssh://git@forge.example/<org>); the repo is the exact
   repository name. Release steps then read
   \$OBERTH_SECRETSTORE_DIR/github-token/token. The values are fetched at
   release admission — a missing secret or a path scoped to another org or
   repository fails the run before its Job starts — and are delivered only
   into the release Job's memory.

4. Prove the whole trust chain end to end from inside the Oberth pod
   (ServiceAccount login, TokenReview, read policy, TLS — no credentials
   needed beyond the pod's own identity):

     kubectl exec -n $NAMESPACE deploy/oberth -- \\
       oberth secretstore verify oberth/upstream/<org>/<repo>/github-token
SUMMARY
