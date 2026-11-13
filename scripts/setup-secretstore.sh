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
#   4. writes a minimal KV-read and token-revocation policy
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
# Requirements: bao or vault CLI (authenticated), python3, and a configured
# file audit device. kubectl (read-only) when run
# outside the cluster; inside a pod the mounted ServiceAccount CA is used.
#
# This canonical script is embedded in the server and can be extracted with
# `oberth secretstore setup --print-script`.

set -euo pipefail

MOUNT="kubernetes"
ROLE="oberth-ci"
POLICY="oberth-ci"
KV_PREFIX="oberth"
NAMESPACE="oberth"
SERVICE_ACCOUNT="oberth"
KUBERNETES_HOST=""
KUBE_CONTEXT=""
KUBE_CONFIG=""
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
  --namespace NAME         Oberth Kubernetes namespace       (default: oberth)
  --service-account NAME   Oberth ServiceAccount name        (default: oberth)
  --kubernetes-host URL    API server URL the secret store uses for TokenReview
                           (default: in-cluster service URL when run in-cluster,
                            otherwise the selected kubeconfig server)
  --context NAME           recorded or explicitly selected Kubernetes context
  --kubeconfig PATH        source kubeconfig for that context
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
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --service-account) SERVICE_ACCOUNT="$2"; shift 2 ;;
    --kubernetes-host) KUBERNETES_HOST="$2"; shift 2 ;;
    --context) KUBE_CONTEXT="$2"; shift 2 ;;
    --kubeconfig) KUBE_CONFIG="$2"; shift 2 ;;
    --address) ADDRESS="$2"; shift 2 ;;
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
for required_name in "$MOUNT" "$ROLE" "$POLICY" "$KV_PREFIX" "$NAMESPACE" "$SERVICE_ACCOUNT"; do
  [ -n "$required_name" ] || fail "names must not be empty"
  case "$required_name" in .|..) fail "names must be clean path segments, not . or .." ;; esac
done

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
    warn "secret store address $STORE_ADDR is loopback plain HTTP — acceptable only for local development" ;;
  http://*)
    warn "secret store address $STORE_ADDR uses PLAIN HTTP over the network."
    warn "Oberth requires https:// unless you explicitly set secretstore.insecureHTTPForDev." ;;
  *) fail "secret store address $STORE_ADDR must be an http(s) URL" ;;
esac
SKIP_VERIFY_ENABLED="false"
case "${BAO_SKIP_VERIFY:-}" in 1|t|T|true|TRUE|True) SKIP_VERIFY_ENABLED="true" ;; esac
case "${VAULT_SKIP_VERIFY:-}" in 1|t|T|true|TRUE|True) SKIP_VERIFY_ENABLED="true" ;; esac
if [ "$SKIP_VERIFY_ENABLED" = "true" ]; then
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

# Reads authorize later mutations only after a value or explicit path-specific
# absence is verified. Exit 2 also covers permission denied, transport and
# server errors; neither the status alone nor empty output proves absence.
read_field() {
  local _rf_out _rf_exit
  if _rf_out="$("$CLI" read -field="$1" "$2" 2>&1)"; then
    [ -n "$_rf_out" ] || fail "reading $2 field=$1 returned an empty value; refusing setup"
    printf '%s' "$_rf_out"
    return 0
  else
    _rf_exit=$?
  fi
  if [ "$_rf_exit" -eq 2 ] && [ "$_rf_out" = "No value found at $2" ]; then
    return 0
  fi
  fail "reading $2 field=$1 failed (exit $_rf_exit): $_rf_out"
}

# Mount reads use different absence diagnostics from object reads. Only a
# successful, valid mount inventory can authorize enabling an absent mount.
read_mount_type() {
  local _mt_out _mt_exit
  if _mt_out="$("$CLI" "$1" list -format=json 2>&1)"; then
    :
  else
    _mt_exit=$?
    fail "listing $1 mounts failed (exit $_mt_exit): $_mt_out"
  fi
  python3 -I -c '
import json, sys
try:
    mounts = json.load(sys.stdin)
    if not isinstance(mounts, dict):
        raise ValueError()
    for mount in mounts.values():
        if not isinstance(mount, dict) or not isinstance(mount.get("type"), str) or not mount["type"].strip():
            raise ValueError()
    if sys.argv[1] in mounts:
        print(mounts[sys.argv[1]]["type"])
except (ValueError, TypeError):
    sys.exit(1)
' "$2/" <<< "$_mt_out" || fail "invalid $1 mount list JSON; refusing setup"
}

read_policy() {
  local _rp_out _rp_exit
  if _rp_out="$("$CLI" policy read "$1" 2>&1)"; then
    [ -n "$_rp_out" ] || fail "reading policy $1 returned empty output; refusing setup"
    printf '%s' "$_rp_out"
    return 0
  else
    _rp_exit=$?
  fi
  if [ "$_rp_exit" -eq 2 ] && [ "$_rp_out" = "No policy named: $1" ]; then
    return 0
  fi
  fail "reading policy $1 failed (exit $_rp_exit): $_rp_out"
}

# --- audit preflight (read-only; before any store mutation) ------------------
# File devices are configured by the store operator. Do not enable unsafe API
# audit creation or mistake a CLI/JSON error for an absent device.
command -v python3 >/dev/null 2>&1 || fail "python3 is required to validate audit device JSON"
AUDIT_ENABLED="$("$CLI" audit list -format=json)" || fail "could not list audit devices; verify CLI permission and connectivity"
audit_status=0
python3 -I -c '
import json, sys
try:
    devices = json.load(sys.stdin)
    if not isinstance(devices, dict):
        raise ValueError()
    found = False
    for device in devices.values():
        if not isinstance(device, dict) or not isinstance(device.get("type"), str):
            raise ValueError()
        options = device.get("options", {})
        if not isinstance(options, dict):
            raise ValueError()
        if device["type"] == "file":
            sink = options.get("file_path")
            if not isinstance(sink, str):
                raise ValueError()
            if sink.strip() and sink not in ("discard", "/dev/null"):
                found = True
except (ValueError, TypeError):
    sys.exit(2)
sys.exit(0 if found else 1)
' <<< "$AUDIT_ENABLED" || audit_status=$?
case "$audit_status" in
  0) note "file audit device already enabled — keeping it" ;;
  1) fail 'configure a file audit device in the OpenBao server configuration before rerunning setup (for example: audit "file" "oberth" { options { file_path = "/openbao/data/audit.log" } }); use a writable persistent path or a collected stdout sink, never discard; do not enable unsafe_allow_api_audit_creation' ;;
  *) fail "invalid audit device JSON; refusing setup without verified file auditing" ;;
esac
unset AUDIT_ENABLED
# --- end audit preflight -----------------------------------------------------

# --- cluster facts (read-only) ----------------------------------------------
CLUSTER_CA=""
if [ -z "$KUBE_CONTEXT$KUBE_CONFIG" ] && [ -n "${KUBERNETES_SERVICE_HOST:-}" ] && [ -r "$IN_CLUSTER_CA" ]; then
  # Running inside the cluster (e.g. next to an in-cluster OpenBao): the store
  # reaches the API server through the in-cluster service address, and the
  # pod's mounted ServiceAccount bundle already carries the cluster CA —
  # kubectl is not needed at all.
  [ -n "$KUBERNETES_HOST" ] || KUBERNETES_HOST="https://kubernetes.default.svc"
  CLUSTER_CA="$(cat "$IN_CLUSTER_CA")"
else
  command -v kubectl >/dev/null 2>&1 || fail "kubectl not found in PATH (read-only use; needed to derive the API server URL and CA)"
  CLUSTER_FACTS="$(python3 - "$KUBE_CONTEXT" "$KUBE_CONFIG" <<'PYTHON'
import base64, hashlib, json, os, pathlib, stat, subprocess, sys
context, config = sys.argv[1:]
record_path = pathlib.Path(os.environ.get("XDG_CONFIG_HOME", str(pathlib.Path.home()/".config"))) / "oberth/kubernetes.json"
record = None
try:
    if record_path.exists():
        info = record_path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_size > 65536:
            raise ValueError("unsafe recorded Kubernetes target")
        record = json.loads(record_path.read_text())
        context = context or record["context"]
        config = config or record.get("kubeconfig", "")
    if not context:
        raise ValueError("select --context or use the recorded install target")
    command = ["kubectl", "--context", context]
    if config:
        command += ["--kubeconfig", config]
    command += ["config", "view", "--minify", "--flatten", "--raw", "-o", "json"]
    selected = json.loads(subprocess.check_output(command, stderr=subprocess.PIPE))
    cluster = selected["clusters"][0]["cluster"]
    server = cluster["server"]
    ca = base64.b64decode(cluster.get("certificate-authority-data", ""), validate=True)
    if record and context == record["context"]:
        if (server != record.get("api_server") or hashlib.sha256(ca).hexdigest() != record.get("ca_sha256")
                or cluster.get("tls-server-name", "") != record.get("tls_server_name", "")
                or cluster.get("insecure-skip-tls-verify", False) != record.get("insecure_tls", False)):
            raise ValueError("recorded Kubernetes cluster identity changed (API server or CA)")
    if not ca:
        raise ValueError("selected Kubernetes context must provide a CA bundle for TokenReview")
    print(context)
    print(server)
    print(base64.b64encode(ca).decode())
except (ValueError, KeyError, IndexError, OSError, subprocess.CalledProcessError):
    sys.exit("cannot verify selected Kubernetes cluster; check --context, API server and CA against the install record")
PYTHON
)" || fail "could not verify Kubernetes cluster identity"
  mapfile -t CLUSTER_FACT_ARRAY <<< "$CLUSTER_FACTS"
  KUBE_CONTEXT="${CLUSTER_FACT_ARRAY[0]}"
  [ -n "$KUBERNETES_HOST" ] || KUBERNETES_HOST="${CLUSTER_FACT_ARRAY[1]}"
  CLUSTER_CA="$(printf '%s' "${CLUSTER_FACT_ARRAY[2]}" | base64 -d)"
  note "using kubeconfig context: $KUBE_CONTEXT"

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
AUTH_TYPE="$(read_mount_type auth "$MOUNT")"
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
# permits that TokenReview for the server and exact pipeline login identities);
# an in-cluster store uses its own ServiceAccount. Set secretstore.external=true
# for external stores behind a cluster-local DNS alias.
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
KV_TYPE="$(read_mount_type secrets "$KV_PREFIX")"
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

# --- 5. minimal KV data policy -------------------------------
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
  POLICY_DESCRIPTION="KV read and token revocation"
HAVE_POLICY="$(read_policy "$POLICY")"
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
for short-lived access to $KV_PREFIX/data/*. That KV read grant covers
both Oberth secret namespaces on this mount; Oberth enforces the org/repo
scoping below at release admission, per triggering repository:

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
       --set secretstore.kvMount=$KV_PREFIX



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

4. Prove the whole trust chain end to end through the authenticated admin API
   (ServiceAccount login, TokenReview, read policy, TLS), using your saved
   unscoped admin identity:

     oberth mcp call secretstore_verify '{"paths":["oberth/upstream/<org>/<repo>/github-token"]}'
SUMMARY
