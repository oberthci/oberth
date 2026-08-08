#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
values=${1:-"$repo_root/charts/oberth/values.yaml"}
oci=${2:-"$repo_root/dist/oberth-runner.oci"}
receipt=${3:-"$repo_root/artifacts/runner-image-receipt.json"}
scan=${4:-"$repo_root/artifacts/runner-image-scan.json"}
state=${5:-"$repo_root/artifacts/runner-image-state.json"}
verify_tmpdir=${RUNNER_VERIFY_TMPDIR:-/var/tmp}

fail() {
  echo "runner pin gate: $*" >&2
  exit 1
}

for command_name in flock python3 git jq sha256sum tar find readlink mktemp date grep wc tr awk cp chmod mkdir stat id; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done

# Deterministic content digest of the normalized context tree. The recipe uses
# only portable find/stat/sha256sum semantics so it computes identically on GNU
# hosts and the minimal BusyBox runner substrate, which has no GNU tar
# --sort/--owner/--mtime. It must stay byte-identical to context_tree_sha in
# hack/verify-runner-image.sh; the receipt's contextSHA256 binds them.
context_tree_sha() {
  (
    cd "$1" || exit 1
    find . -mindepth 1 | LC_ALL=C sort | while IFS= read -r entry; do
      if test -L "$entry"; then
        printf 'link %s %s\n' "$entry" "$(readlink "$entry")"
      elif test -d "$entry"; then
        printf 'dir %s %s\n' "$(stat -c '%a' "$entry")" "$entry"
      else
        printf 'file %s %s %s\n' "$(stat -c '%a' "$entry")" "$(sha256sum "$entry" | cut -d ' ' -f 1)" "$entry"
      fi
    done
  ) | sha256sum | cut -d ' ' -f 1
}

# Parse a UTC RFC 3339 timestamp to epoch seconds portably: BusyBox date
# rejects the T/Z form, so normalize to "YYYY-MM-DD HH:MM:SS" and refuse
# non-UTC offsets outright.
utc_iso_to_epoch() {
  case "$1" in
    *Z|*+00:00) ;;
    *) echo "timestamp is not UTC: $1" >&2; return 1 ;;
  esac
  date -u -d "$(printf '%s' "$1" | sed 's/T/ /; s/\.[0-9]*//; s/Z$//; s/+00:00$//')" +%s
}

if test -L "$verify_tmpdir" || test ! -d "$verify_tmpdir"; then
  fail "verification temporary directory must be a directory: $verify_tmpdir"
fi
verify_tmpdir_mode=$(stat -c '%a' -- "$verify_tmpdir") || fail "cannot inspect verification temporary directory"
case "$verify_tmpdir_mode" in
  ''|*[!0-7]*) fail "verification temporary directory has an invalid mode" ;;
esac
verify_tmpdir_bits=$((0$verify_tmpdir_mode))
if test $((verify_tmpdir_bits & 0022)) -ne 0 && test $((verify_tmpdir_bits & 01000)) -eq 0; then
  fail "verification temporary directory must not be group/world writable without the sticky bit"
fi
repo_key=$(printf '%s' "$repo_root" | sha256sum | cut -d ' ' -f 1)
lock_dir=$verify_tmpdir/oberth-runner-locks-$repo_key
if ! mkdir -m 0700 -- "$lock_dir" 2>/dev/null; then
  if test -L "$lock_dir" || test ! -d "$lock_dir"; then
    fail "runner lock directory must be a private directory"
  fi
fi
test "$(stat -c '%u:%a' -- "$lock_dir")" = "$(id -u):700" || \
  fail "runner lock directory must be owned by the current user with mode 0700"
lock_file=$lock_dir/evidence.lock
test ! -L "$lock_file" || fail "runner evidence lock must not be a symlink"
if test -e "$lock_file" && test ! -f "$lock_file"; then
  fail "runner evidence lock must be a regular file"
fi
exec 9>"$lock_file" || fail "cannot open runner evidence lock"
flock -s -n 9 || fail "runner image verification is still in progress"

work_dir=$(mktemp -d "$verify_tmpdir/oberth-runner-pin.XXXXXX")
cleanup() {
  trap - EXIT HUP INT TERM
  find "$work_dir" -depth -delete >/dev/null 2>&1
}
trap cleanup EXIT
trap 'trap - HUP; exit 129' HUP
trap 'trap - INT; exit 130' INT
trap 'trap - TERM; exit 143' TERM

for required in "$values" "$oci" "$receipt" "$scan" "$state" "$repo_root/hack/runner-oci.py"; do
  if test ! -f "$required" || test -L "$required"; then
    fail "required evidence is not a regular file: $required"
  fi
done
oci_helper=$repo_root/hack/runner-oci.py
oci_helper_sha=$(sha256sum "$oci_helper" | cut -d ' ' -f 1)
cp -- "$oci_helper" "$work_dir/runner-oci.py"
chmod 0500 "$work_dir/runner-oci.py"
trusted_oci_helper=$work_dir/runner-oci.py
test "$(sha256sum "$trusted_oci_helper" | cut -d ' ' -f 1)" = "$oci_helper_sha" || \
  fail "private OCI helper copy failed checksum verification"
test -z "$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all)" || \
  fail "deployment requires a clean source tree"

receipt_sha=$(sha256sum "$receipt" | cut -d ' ' -f 1)
if ! jq -e --arg receipt_sha "$receipt_sha" \
  '.schema == 1 and .status == "passed" and .exitCode == 0 and
   .receiptSHA256 == $receipt_sha' "$state" >/dev/null; then
  fail "latest verification attempt is not the passing receipt"
fi
if ! jq -e \
  '.schema == 1 and .passed == true and .source.dirty == false and
   .scanner.exitCode == 0 and .scanner.high == 0 and
   .scanner.critical == 0 and .scanner.secrets == 0' "$receipt" >/dev/null; then
  fail "receipt is not clean and passing"
fi

source_revision=$(jq -er '.source.revision' "$receipt")
source_date_epoch=$(jq -er '.source.sourceDateEpoch' "$receipt")
printf '%s\n' "$source_date_epoch" | grep -Eq '^[0-9]+$' || fail "invalid source epoch"
git -C "$repo_root" merge-base --is-ancestor "$source_revision" HEAD || \
  fail "receipt source is not an ancestor of the deployment"
gate_sha=$(sha256sum "$repo_root/hack/verify-runner-image.sh" | cut -d ' ' -f 1)
test "$gate_sha" = "$(jq -er '.source.gateSHA256' "$receipt")" || \
  fail "runner image gate changed after the receipt"

mkdir -p -- "$work_dir/context"
tar -C "$repo_root" -cf "$work_dir/context.tar" -- \
  Dockerfile.runner Dockerfile.runner.dockerignore go.mod go.sum \
  cmd/oberth-runner internal/gitoid internal/runner pkg/periapsis
tar -C "$work_dir/context" -xf "$work_dir/context.tar"
find "$work_dir/context" -type d -exec chmod 0755 {} +
find "$work_dir/context" -type f -perm -0100 -exec chmod 0755 {} +
find "$work_dir/context" -type f ! -perm -0100 -exec chmod 0644 {} +
context_sha=$(context_tree_sha "$work_dir/context")
receipt_context_sha=$(jq -er '.source.contextSHA256' "$receipt")
test "$context_sha" = "$receipt_context_sha" || \
  fail "current runner source context is $context_sha, receipt expects $receipt_context_sha"
test "$context_sha" = "$(jq -er '.sourceContextSHA256' "$state")" || \
  fail "attempt state and receipt describe different source contexts"
dockerfile_sha=$(sha256sum "$repo_root/Dockerfile.runner" | cut -d ' ' -f 1)
test "$dockerfile_sha" = "$(jq -er '.image.dockerfileSHA256' "$receipt")" || \
  fail "Dockerfile.runner does not match the receipt"

updated_at=$(jq -er '.scanner.databaseUpdatedAt' "$receipt")
updated_epoch=$(utc_iso_to_epoch "$updated_at")
database_age=$(($(date -u +%s) - updated_epoch))
if test "$database_age" -lt -900 || test "$database_age" -gt 259200; then
  fail "receipt vulnerability database is outside the 72-hour window"
fi

scan_sha=$(sha256sum "$scan" | cut -d ' ' -f 1)
test "$scan_sha" = "$(jq -er '.scanner.reportSHA256' "$receipt")" || \
  fail "normalized scan report hash does not match the receipt"
test "$(basename -- "$scan")" = "$(jq -er '.scanner.reportFile' "$receipt")" || \
  fail "receipt points at a different scan report"

archive_sha=$(sha256sum "$oci" | cut -d ' ' -f 1)
test "$archive_sha" = "$(jq -er '.image.archiveSHA256' "$receipt")" || \
  fail "OCI archive hash does not match the receipt"
if ! python3 -I -S -B "$trusted_oci_helper" extract "$oci" "$work_dir/oci-layout" "local OCI evidence"; then
  fail "OCI archive extraction failed"
fi
if ! python3 -I -S -B "$trusted_oci_helper" validate "$work_dir/oci-layout" - "local OCI evidence" >"$work_dir/oci-summary.json"; then
  fail "OCI graph validation failed"
fi
manifest_digest=$(jq -er '.manifest' "$work_dir/oci-summary.json")
config_digest=$(jq -er '.configDigest' "$work_dir/oci-summary.json")
layer_count=$(jq -er '.layerCount' "$work_dir/oci-summary.json")
printf '%s\n' "$manifest_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || fail "OCI manifest digest is invalid"
printf '%s\n' "$config_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || fail "OCI config digest is invalid"
printf '%s\n' "$layer_count" | grep -Eq '^[1-9][0-9]*$' || fail "OCI layer count is invalid"
test "$manifest_digest" = "$(jq -er '.image.manifest' "$receipt")" || \
  fail "OCI manifest does not match the receipt"
test "$config_digest" = "$(jq -er '.image.configDigest' "$receipt")" || \
  fail "OCI config does not match the receipt"
test "$layer_count" = "$(jq -er '.image.layerCount' "$receipt")" || \
  fail "OCI layer count does not match the receipt"
manifest_blob=$work_dir/oci-layout/blobs/sha256/${manifest_digest#sha256:}
compressed_layer_bytes=$(jq -er '[.layers[].size] | add' "$manifest_blob")
test "$compressed_layer_bytes" = "$(jq -er '.image.compressedLayerBytes' "$receipt")" || \
  fail "OCI compressed layer size does not match the receipt"
if test "$compressed_layer_bytes" -le 0 || test "$compressed_layer_bytes" -gt 104857600; then
  fail "runner image exceeds the 100 MiB compressed-layer limit"
fi
config_blob=$work_dir/oci-layout/blobs/sha256/${config_digest#sha256:}
config_json=$(cat "$config_blob")
platform=$(printf '%s' "$config_json" | jq -er '.os + "/" + .architecture')
image_user=$(printf '%s' "$config_json" | jq -er '.config.User')
image_created=$(printf '%s' "$config_json" | jq -er '.created')
test "$platform" = linux/amd64 || fail "runner platform is not linux/amd64"
test "$image_user" = 65534:65534 || fail "runner image user is not 65534:65534"
test "$image_created" = "$(date -u -d "@$source_date_epoch" +%Y-%m-%dT%H:%M:%SZ)" || \
  fail "runner image timestamp is not source-derived"

runner_ref=$(awk '
  /^runnerImage:[[:space:]]*$/ { in_runner = 1; next }
  in_runner && /^[^[:space:]]/ { exit }
  in_runner && /^[[:space:]]+ref:/ {
    sub(/^[[:space:]]+ref:[[:space:]]*/, "")
    gsub(/^"|"$/, "")
    print
    exit
  }
' "$values")
test -n "$runner_ref" || fail "chart has no runnerImage.ref"
test "${runner_ref##*@}" = "$manifest_digest" || \
  fail "chart runner digest does not match the passing receipt"

if ! jq -e --arg manifest "$manifest_digest" --arg image_id "$config_digest" \
  '.ArtifactName == $manifest and .ArtifactType == "container_image" and
   .Metadata.ImageID == $image_id' "$scan" >/dev/null; then
  fail "scan report is not bound to the verified OCI"
fi
expected_target=usr/local/bin/oberth-runner
jq -e --arg target "$expected_target" \
  'any(.Results[]?; .Class == "lang-pkgs" and .Type == "gobinary" and
    (.Target | endswith($target)) and ((.Packages // []) | length) > 0)' \
  "$scan" >/dev/null || fail "scan report omitted $expected_target"
for forbidden_target in golangci-lint cosign trivy helm etcd kube-apiserver kubectl; do
  if jq -e --arg target "$forbidden_target" \
    'any(.Results[]?; .Class == "lang-pkgs" and .Type == "gobinary" and
      (.Target | endswith($target)) and ((.Packages // []) | length) > 0)' \
    "$scan" >/dev/null; then
    fail "scan report contains forbidden baked tool $forbidden_target"
  fi
done
for forbidden_package in python3; do
  if jq -e --arg package "$forbidden_package" \
    'any(.Results[]? | select(.Class == "os-pkgs") | .Packages[]?; .Name == $package)' \
    "$scan" >/dev/null; then
    fail "scan report contains forbidden baked package $forbidden_package"
  fi
done
if jq -e 'any(.Results[]?; .Type == "python-pkg")' "$scan" >/dev/null; then
  fail "scan report contains forbidden Python package inventory"
fi

result_count=$(jq '[.Results[]?] | length' "$scan")
package_count=$(jq '[.Results[]?.Packages[]?] | length' "$scan")
os_target_count=$(jq '[.Results[]? | select(.Class == "os-pkgs" and .Type == "alpine" and ((.Packages // []) | length) > 0)] | length' "$scan")
go_target_count=$(jq '[.Results[]? | select(.Class == "lang-pkgs" and .Type == "gobinary" and ((.Packages // []) | length) > 0)] | length' "$scan")
high=$(jq '[.Results[]?.Vulnerabilities[]? | select(.Severity == "HIGH")] | length' "$scan")
critical=$(jq '[.Results[]?.Vulnerabilities[]? | select(.Severity == "CRITICAL")] | length' "$scan")
secrets=$(jq '[.Results[]?.Secrets[]?] | length' "$scan")
if ! jq -e \
  --argjson results "$result_count" --argjson packages "$package_count" \
  --argjson os_targets "$os_target_count" --argjson go_targets "$go_target_count" \
  --argjson high "$high" --argjson critical "$critical" --argjson secrets "$secrets" \
  '.scanner.resultCount == $results and .scanner.packageCount == $packages and
   .scanner.osTargetCount == $os_targets and .scanner.goBinaryTargetCount == $go_targets and
   .scanner.high == $high and .scanner.critical == $critical and .scanner.secrets == $secrets and
   $results >= 2 and $packages >= 10 and $os_targets >= 1 and $go_targets == 1 and
   $high == 0 and $critical == 0 and $secrets == 0' "$receipt" >/dev/null; then
  fail "scan coverage or findings do not match the receipt"
fi
