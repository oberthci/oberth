#!/usr/bin/env bash
# Contract test for scripts/setup-secretstore.sh against mock bao/kubectl CLIs.
# Pins the script's security contract: idempotent re-runs mutate nothing,
# drifted objects are refused without --force, a config naming another cluster
# is refused without --force-auth-config, type/version mismatches fail closed,
# --dry-run performs no mutation, CLI TLS posture is reported, and the
# in-cluster branch works without kubectl. Run by TestSetupSecretStore
# ScriptContract (embed_test.go) in every `go test ./...`.
#
# shellcheck disable=SC2016  # check() arguments are eval'd later by design.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
script=$root/scripts/setup-secretstore.sh

work=$(mktemp -d)
trap 'find "$work" -delete' EXIT
bin=$work/bin
mkdir -p "$bin"

# --- mock CLIs ---------------------------------------------------------------
cat >"$bin/bao" <<'MOCK'
#!/usr/bin/env bash
set -eu
state="$MOCK_STATE"
printf 'bao %s\n' "$*" >>"$state/calls.log"
if [ "$*" = "${MOCK_FAIL_COMMAND:-}" ]; then
  printf '%s' "${MOCK_FAIL_OUTPUT:-}" >&2
  exit "${MOCK_FAIL_EXIT:-2}"
fi
absent() { printf 'No value found at %s\n' "$path" >&2; exit 2; }
absent_mount() {
  printf 'Error reading %s: Error making API request.\n\nCode: 400. Errors:\n\n* %s at %s/\n' "$path" "$1" "${path##*/}" >&2
  exit 2
}
cmd="${1:-}"; shift || true
case "$cmd" in
  token) exit 0 ;;
  audit)
    [ "${1:-}" = list ] || exit 91
    if [ -f "$state/audit-read-error" ]; then
      printf 'synthetic audit permission denied\n' >&2
      exit 37
    fi
    cat "$state/audit.json"
    exit 0 ;;
  auth)
    if [ "${1:-}" = "list" ]; then
      python3 -c 'import json, pathlib, sys; p = pathlib.Path(sys.argv[1]); print(json.dumps({"kubernetes/": {"type": p.read_text().strip()}} if p.exists() else {}))' "$state/auth-type"
    fi
    if [ "${1:-}" = "enable" ]; then printf 'kubernetes\n' >"$state/auth-type"; fi
    exit 0 ;;
  secrets)
    if [ "${1:-}" = "list" ]; then
      python3 -c 'import json, pathlib, sys; p = pathlib.Path(sys.argv[1]); print(json.dumps({mount + "/": {"type": (p / name).read_text().strip()} for mount, name in [("oberth", "kv-type")] if (p / name).exists()}))' "$state"
    fi
    if [ "${1:-}" = "enable" ]; then
      printf 'kv\n' >"$state/kv-type"
      printf '2\n' >"$state/kv-version"
    fi
    exit 0 ;;
  read)
    field=""; path=""
    for argument in "$@"; do
      case "$argument" in -field=*) field="${argument#-field=}" ;; *) path="$argument" ;; esac
    done
    role_prefix=role-
    case "$path" in
      auth/*/role/*) [ "${path##*/}" = oberth-ci ] || role_prefix="${path##*/}-" ;;
    esac
    case "$field:$path" in
      type:sys/auth/*) [ -f "$state/auth-type" ] && cat "$state/auth-type" || absent_mount 'No auth engine' ;;
      kubernetes_host:auth/*/config) [ -f "$state/config-host" ] && cat "$state/config-host" || absent ;;
      type:sys/mounts/*) [ -f "$state/kv-type" ] && cat "$state/kv-type" || absent_mount 'No secret engine mount' ;;
      options:sys/mounts/*)
        [ -f "$state/kv-version" ] || absent_mount 'No secret engine mount'
        printf 'map[version:%s]\n' "$(cat "$state/kv-version")" ;;
      bound_service_account_names:auth/*/role/*) [ -f "$state/${role_prefix}names" ] && cat "$state/${role_prefix}names" || absent ;;
      bound_service_account_namespaces:auth/*/role/*) [ -f "$state/${role_prefix}namespaces" ] && cat "$state/${role_prefix}namespaces" || absent ;;
      token_policies:auth/*/role/*) [ -f "$state/${role_prefix}policies" ] && cat "$state/${role_prefix}policies" || absent ;;
      token_no_default_policy:auth/*/role/*) [ -f "$state/${role_prefix}no-default" ] && cat "$state/${role_prefix}no-default" || absent ;;
      token_ttl:auth/*/role/*) [ -f "$state/${role_prefix}ttl" ] && cat "$state/${role_prefix}ttl" || absent ;;
      token_max_ttl:auth/*/role/*) [ -f "$state/${role_prefix}max-ttl" ] && cat "$state/${role_prefix}max-ttl" || absent ;;
      *) absent ;;
    esac ;;
  write)
    target="${1:-}"; shift || true
    case "$target" in
      auth/*/config)
        for argument in "$@"; do
          case "$argument" in kubernetes_host=*) printf '%s\n' "${argument#kubernetes_host=}" >"$state/config-host" ;; esac
        done ;;
      auth/*/role/*)
        role_prefix=role-
        [ "${target##*/}" = oberth-ci ] || role_prefix="${target##*/}-"
        for argument in "$@"; do
          case "$argument" in
            bound_service_account_names=*) printf '[%s]\n' "${argument#*=}" >"$state/${role_prefix}names" ;;
            bound_service_account_namespaces=*) printf '[%s]\n' "${argument#*=}" >"$state/${role_prefix}namespaces" ;;
            token_policies=*) printf '[%s]\n' "${argument#*=}" >"$state/${role_prefix}policies" ;;
            token_no_default_policy=*) printf '%s\n' "${argument#*=}" >"$state/${role_prefix}no-default" ;;
            token_ttl=*) printf '%s\n' "${argument#*=}" >"$state/${role_prefix}ttl" ;;
            token_max_ttl=*) printf '%s\n' "${argument#*=}" >"$state/${role_prefix}max-ttl" ;;
          esac
        done ;;
    esac
    exit 0 ;;
  policy)
    sub="${1:-}"; name="${2:-}"
    if [ "$sub" = "read" ]; then
      if [ -f "$state/policy-$name" ]; then
        cat "$state/policy-$name"
      else
        printf 'No policy named: %s\n' "$name" >&2
        exit 2
      fi
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
  *"config view"*) printf '%s\n' '{"clusters":[{"cluster":{"server":"https://kube.example:6443","certificate-authority-data":"LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUZBS0VURVNUQ0EKLS0tLS1FTkQgQ0VSVElGSUNBVEUtLS0tLQo="}}]}' ;;
  *) exit 1 ;;
esac
MOCK
chmod +x "$bin/bao" "$bin/kubectl"

export PATH="$bin:$PATH"
unset VAULT_ADDR VAULT_SKIP_VERIFY BAO_SKIP_VERIFY KUBERNETES_SERVICE_HOST IN_CLUSTER_CA 2>/dev/null || true
export BAO_ADDR="https://store.example:8200"
export XDG_CONFIG_HOME="$work/config"
mkdir -p "$XDG_CONFIG_HOME/oberth"
python3 - "$XDG_CONFIG_HOME/oberth/kubernetes.json" <<'PYTHON'
import hashlib,json,pathlib,sys
ca=b"-----BEGIN CERTIFICATE-----\nMIIFAKETESTCA\n-----END CERTIFICATE-----\n"
p=pathlib.Path(sys.argv[1]);p.write_text(json.dumps({"context":"recorded-context","namespace":"oberth","api_server":"https://kube.example:6443","ca_sha256":hashlib.sha256(ca).hexdigest()}));p.chmod(0o600)
PYTHON


failures=0
scenario=""
fresh_state() {
  MOCK_STATE=$(mktemp -d "$work/state.XXXXXX")
  export MOCK_STATE
  : >"$MOCK_STATE/calls.log"
  cat >"$MOCK_STATE/audit.json" <<'JSON'
{
  "existing-production-audit/": {
    "description": "fixture based on real Bao CLI JSON",
    "local": false,
    "options": {"file_path": "/openbao/data/audit.log"},
    "type": "file"
  }
}
JSON
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
check 'contains "writing policy oberth-ci"' "must write the policy"
check 'contains "writing role oberth-ci bound to ServiceAccount oberth/oberth"' "must write the role"
check 'contains "secretstore.enabled=true"' "helm handoff must enable the secret store"
check 'contains "secretstore.address=https://store.example:8200"' "helm handoff must carry the address"
check 'contains "oberth mcp call secretstore_verify"' "next steps must include authenticated API verification"
if ! python3 -c 'import json,shlex,sys
lines=sys.stdin.read().splitlines()
command=next(line.strip() for line in lines if line.strip().startswith("oberth mcp call secretstore_verify"))
args=shlex.split(command)
assert len(args)==5 and json.loads(args[4])=={"paths":["oberth/upstream/<org>/<repo>/github-token"]}
' <<< "$output"; then
  check false 'printed verification command must pass paths inside its sole JSON argument'
fi


check 'calls_contain "bao auth enable -path=kubernetes kubernetes"' "auth enable must reach the CLI"
check 'calls_contain "token_no_default_policy=true"' "role must drop the default policy"
check 'grep -Fq "auth/token/revoke-self" "$MOCK_STATE/policy-oberth-ci"' "policy must allow the client to revoke its own login token"
check '! grep -Fq "oberth-transit/" "$MOCK_STATE/policy-oberth-ci"' "policy must not grant Transit access"
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

begin "extra-role-binding"
printf '[oberth,other-service-account]\n' >"$MOCK_STATE/role-names"
run_script
check '[ "$status" != 0 ]' "an extra ServiceAccount binding must fail closed"
check 'contains "different binding or token lifetime"' "role drift must identify the binding/lifetime boundary"
printf '[oberth]\n' >"$MOCK_STATE/role-names"

begin "permissive-role-ttl"
printf '1h\n' >"$MOCK_STATE/role-ttl"
run_script
check '[ "$status" != 0 ]' "a permissive role TTL must fail closed"
check 'contains "different binding or token lifetime"' "TTL drift must identify the binding/lifetime boundary"
printf '10m\n' >"$MOCK_STATE/role-ttl"

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

# --- 8. plain HTTP requires explicit KV-only mode ----------------------------
begin "http-warning"
fresh_state
BAO_ADDR="http://store.example:8200" run_script
BAO_ADDR="http://store.example:8200" run_script
check '[ "$status" = 0 ]' "explicit KV-only HTTP development setup must succeed (status=$status)"
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
check '[ "$status" = 0 ]' "plain-HTTP --address with explicit KV-only mode must succeed (status=$status)"
check 'contains "PLAIN HTTP"' "plain-HTTP --address must be subject to the posture warning"

begin "skip-verify-warning"
fresh_state
BAO_SKIP_VERIFY="true" run_script
check '[ "$status" = 0 ]' "KV setup retains its explicit CLI posture warning"
check 'contains "TLS verification is DISABLED"' "warning must name the disabled-verification boundary"
BAO_SKIP_VERIFY="false" run_script
check '[ "$status" = 0 ]' "an explicitly false skip-verify setting must remain verified (status=$status)"

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

# --- 11. actual CLI-shaped audit preflight -----------------------------------
# Execute the maintained preflight verbatim with the same mock CLI.
candidate=$root/scripts/setup-secretstore.sh
preflight="$work/audit-preflight.sh"
{
  printf '%s\n' '#!/usr/bin/env bash' 'set -euo pipefail' 'CLI=bao' \
    'note() { printf "%s\n" "$*"; }' \
    'fail() { printf "%s\n" "$*" >&2; exit 1; }'
  sed -n '/^# --- audit preflight /,/^# --- end audit preflight /p' "$candidate"
} >"$preflight"
saved_script=$script
script=$preflight
chmod +x "$script"
begin "audit-preflight-${candidate#"$root"/}"
fresh_state
run_script
check '[ "$status" = 0 ]' "pretty CLI JSON with a named file device must pass"
check 'contains "file audit device already enabled"' "the actual preflight must run"
check '! calls_contain "bao audit enable"' "existing audit must never be re-enabled"
printf '%s\n' '{"stdout/":{"type":"file","options":{"file_path":"stdout"}}}' >"$MOCK_STATE/audit.json"
run_script
check '[ "$status" = 0 ]' "configured stdout audit must remain accepted"
for absent in '{}' '{"socket/":{"type":"socket","options":{}}}' \
  '{"discard/":{"type":"file","options":{"file_path":"discard"}}}' \
  '{"null/":{"type":"file","options":{"file_path":"/dev/null"}}}' \
  '{"empty/":{"type":"file","options":{"file_path":""}}}'; do
  printf '%s\n' "$absent" >"$MOCK_STATE/audit.json"
  run_script
  check '[ "$status" != 0 ]' "no effective file sink must refuse setup"
  check 'contains "configure a file audit device"' "missing sink must give declarative guidance"
  check '! calls_contain "bao audit enable"' "missing sink must not trigger API creation"
done
for malformed in 'invalid json' '[]' '{"bad":null}' \
  '{"bad":{"type":"file","options":[]}}' \
  '{"bad":{"type":"file","options":{"file_path":42}}}'; do
  printf '%s\n' "$malformed" >"$MOCK_STATE/audit.json"
  run_script
  check '[ "$status" != 0 ]' "malformed audit response must refuse setup"
  check 'contains "invalid audit device JSON"' "malformed data must not be reported absent"
done
touch "$MOCK_STATE/audit-read-error"
run_script
check '[ "$status" != 0 ]' "audit read failure must refuse setup"
check 'contains "synthetic audit permission denied"' "CLI error must be preserved"
check 'contains "could not list audit devices"' "read failure must not be reported absent"
check '! calls_contain "bao audit enable"' "errors must never trigger API creation"
script=$saved_script

begin "audit-before-mutation"
fresh_state
printf '{}\n' >"$MOCK_STATE/audit.json"
run_script
check '[ "$status" != 0 ]' "missing audit device must refuse the full setup"
check '! calls_contain "bao auth enable"' "audit preflight must precede auth mutation"
check '! calls_contain "bao write"' "audit preflight must precede store writes"
check '! calls_contain "bao policy write"' "audit preflight must precede policy writes"

# The shipped helper must discover all three absent mounts from checked JSON
# lists, without relying on the CLI's different missing-mount read diagnostics.
script=$candidate
begin "fresh-mounts-${candidate#"$root"/}"
fresh_state
run_script
check '[ "$status" = 0 ]' 'fresh store must configure successfully'
check 'calls_contain "bao auth list -format=json"' 'auth discovery must use JSON'
check 'calls_contain "bao secrets list -format=json"' 'secrets discovery must use JSON'
check '[ -f "$MOCK_STATE/auth-type" ] && [ -f "$MOCK_STATE/kv-type" ]' 'fresh setup must enable auth and KV'
check '! calls_contain "bao read -field=type sys/"' 'mount discovery must not probe absent mount paths'
for kind in auth secrets; do
  for failure in denied silent partial invalid null array missing-type empty-type wrong-type; do
    case "$failure" in
      denied) error_exit=2; error_output='Code: 403 permission denied' ;;
      silent) error_exit=1; error_output='' ;;
      partial) error_exit=2; error_output='{}' ;;
      invalid) error_exit=0; error_output='invalid json' ;;
      null) error_exit=0; error_output='null' ;;
      array) error_exit=0; error_output='[]' ;;
      missing-type) error_exit=0; error_output='{"kubernetes/":{}}' ;;
      empty-type) error_exit=0; error_output='{"kubernetes/":{"type":""}}' ;;
      wrong-type) error_exit=0; error_output='{"kubernetes/":{"type":42}}' ;;
    esac
    begin "mount-list-${candidate#"$root"/}-$kind-$failure"
    fresh_state
    MOCK_FAIL_COMMAND="$kind list -format=json" MOCK_FAIL_OUTPUT="$error_output" MOCK_FAIL_EXIT="$error_exit" \
      run_script --force --force-auth-config
    check '[ "$status" != 0 ]' 'unverified mount list must abort even with force flags'
    check 'calls_contain "bao $kind list -format=json"' 'must reach targeted list'
    check '! calls_contain "bao secrets enable"' 'failed list must not enable a secret engine'
    if [ "$kind" = auth ]; then
      check '! calls_contain "bao auth enable"' 'failed auth list must not enable auth'
    fi
  done
done
script=$root/scripts/setup-secretstore.sh

# Failed reads must never become permission to replace an existing object,
# including when the operator deliberately selected a force flag.
saved_script=$script
script=$candidate
for target in config policy role; do
  case "$target" in
    config) read_command='read -field=kubernetes_host auth/kubernetes/config'; mutation='bao write auth/kubernetes/config ' ;;
    policy) read_command='policy read oberth-ci'; mutation='bao policy write oberth-ci ' ;;
    role) read_command='read -field=bound_service_account_names auth/kubernetes/role/oberth-ci'; mutation='bao write auth/kubernetes/role/oberth-ci ' ;;
  esac
  for failure in forbidden timeout tls malformed unknown silent wrong-absence empty-success; do
    case "$failure" in
      forbidden) error_exit=2; error_output='Error reading: Code: 403 permission denied' ;;
      timeout) error_exit=2; error_output='context deadline exceeded' ;;
      tls) error_exit=2; error_output='Error reading: tls: failed to verify certificate: x509: certificate signed by unknown authority' ;;
      malformed) error_exit=2; error_output='Error reading: invalid character after object key in JSON response' ;;
      unknown) error_exit=37; error_output='unexpected CLI failure' ;;
      silent) error_exit=1; error_output='' ;;
      wrong-absence) error_exit=2; error_output='No value found at another/path' ;;
      empty-success) error_exit=0; error_output='' ;;
    esac
    begin "read-error-${candidate#"$root"/}-$target-$failure"
    fresh_state
    # Seed matching server configuration. An unreadable target must not
    # authorize replacement even with --force.
    script=$root/scripts/setup-secretstore.sh
    run_script
    check '[ "$status" = 0 ]' 'fixture setup must succeed'
    script=$candidate
    : >"$MOCK_STATE/calls.log"
    MOCK_FAIL_COMMAND="$read_command" MOCK_FAIL_OUTPUT="$error_output" MOCK_FAIL_EXIT="$error_exit" \
      run_script --force --force-auth-config
    check '[ "$status" != 0 ]' 'unverified read must abort even with force flags'
    check 'calls_contain "bao $read_command"' 'must reach the targeted read'
    if calls_contain "$mutation"; then
      check false 'failed read must not authorize replacement'
    fi
    check 'contains "reading"' 'failure must identify the read error'
    # F1's reported reproduction uses no force flags. Exercise that path
    # directly for the server's config, policy and role.
    if [ "$candidate" = "$root/scripts/setup-secretstore.sh" ]; then
      : >"$MOCK_STATE/calls.log"
      MOCK_FAIL_COMMAND="$read_command" MOCK_FAIL_OUTPUT="$error_output" MOCK_FAIL_EXIT="$error_exit" \
        run_script
      check '[ "$status" != 0 ]' 'unverified read must abort without force flags'
      check 'calls_contain "bao $read_command"' 'must reach the targeted unforced read'
      if calls_contain "$mutation"; then
        check false 'failed unforced read must not authorize replacement'
      fi
      check 'contains "reading"' 'unforced failure must identify the read error'
    fi
  done
done
script=$saved_script

begin "recorded and explicit context"
fresh_state
run_script
check '[ "$status" = 0 ]' 'recorded context must work'
check 'calls_contain "kubectl --context recorded-context config view"' 'must read recorded context, not ambient current-context'
fresh_state
run_script --context explicit-context
check '[ "$status" = 0 ]' 'explicit other context must work'
check 'calls_contain "kubectl --context explicit-context config view"' 'explicit context must win'

for identity_field in api_server ca_sha256; do
  begin "changed recorded $identity_field"
  fresh_state
  cp "$XDG_CONFIG_HOME/oberth/kubernetes.json" "$work/saved-target.json"
  python3 - "$XDG_CONFIG_HOME/oberth/kubernetes.json" "$identity_field" <<'PYTHON'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);record=json.loads(p.read_text());record[sys.argv[2]]="changed";p.write_text(json.dumps(record))
PYTHON
  run_script
  check '[ "$status" != 0 ]' 'changed recorded cluster must be refused'
  check 'contains "could not verify Kubernetes cluster identity"' 'refusal must explain cluster verification'
  if calls_contain 'bao write ' || calls_contain 'bao auth enable '; then
    check false 'changed cluster must authorize no store mutation'
  fi
  cp "$work/saved-target.json" "$XDG_CONFIG_HOME/oberth/kubernetes.json"
done

if [ "$failures" != 0 ]; then
  printf '%d setup-secretstore contract check(s) failed\n' "$failures" >&2
  exit 1
fi
printf 'setup-secretstore contract: OK\n'
