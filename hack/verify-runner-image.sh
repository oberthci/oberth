#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
oci_output=${1:-"$repo_root/dist/oberth-runner.oci"}
receipt_output=${2:-"$repo_root/artifacts/runner-image-receipt.json"}
scan_output=${3:-"$repo_root/artifacts/runner-image-scan.json"}
state_output=${4:-"$repo_root/artifacts/runner-image-state.json"}
verify_tmpdir=${RUNNER_VERIFY_TMPDIR:-/var/tmp}

fail() {
  echo "runner image gate: $*" >&2
  exit 1
}

for command_name in flock sha256sum stat id mkdir mktemp git tar find readlink jq date grep wc tr sed install; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done

# Deterministic content digest of the normalized context tree. The recipe uses
# only portable find/stat/sha256sum semantics so it computes identically on GNU
# hosts and the minimal BusyBox runner substrate, which has no GNU tar
# --sort/--owner/--mtime. Mode normalization above makes the digest independent
# of umask; mtimes and ownership never enter the digest at all.
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
test ! -L "$lock_file" || fail "verification lock must not be a symlink"
if test -e "$lock_file" && test ! -f "$lock_file"; then
  fail "verification lock must be a regular file"
fi
exec 9>"$lock_file" || fail "cannot open verification lock"
flock -n 9 || fail "another verification is already publishing this repository"
work_dir=$(mktemp -d "$verify_tmpdir/oberth-runner-verify.XXXXXX")
oci_publish=
receipt_publish=
scan_publish=
state_publish=
source_revision=
source_status_sha=
context_sha=
verification_started=false
verification_passed=false
if test -n "${RUNNER_TEST_TRIVY:-}"; then
  trivy_bin=$RUNNER_TEST_TRIVY
else
  trivy_bin=$repo_root/bin/trivy
  test ! -L "$trivy_bin" || fail "runner scanner must not be a symlink"
  if test ! -f "$trivy_bin" || test ! -x "$trivy_bin"; then
    fail "run make provision-runner-trivy first"
  fi
  test "$(sha256sum "$trivy_bin" | cut -d ' ' -f 1)" = \
    5b3ebab0f98d95196c85efc3a9d31a01520c96fa342e4e611f56db64c516df1d || \
    fail "runner scanner checksum mismatch"
  test "$("$trivy_bin" --version 2>/dev/null)" = 'Version: 0.73.0' || \
    fail "runner scanner version mismatch"
fi
if test ! -f "$trivy_bin" || test ! -x "$trivy_bin"; then
  fail "runner scanner is not a regular executable"
fi
write_state() {
  state=$1
  state_exit_code=$2
  state_receipt_sha=$3
  jq -n \
    --arg status "$state" \
    --arg revision "$source_revision" \
    --arg context_sha256 "$context_sha" \
    --arg status_sha256 "$source_status_sha" \
    --arg receipt_sha256 "$state_receipt_sha" \
    --argjson exit_code "$state_exit_code" \
    '{
      schema: 1,
      status: $status,
      exitCode: $exit_code,
      sourceRevision: $revision,
      sourceContextSHA256: $context_sha256,
      sourceStatusSHA256: $status_sha256,
      receiptSHA256: $receipt_sha256
    }' >"$work_dir/state.json"
  state_publish=$state_output.tmp.$$
  install -m 0644 "$work_dir/state.json" "$state_publish"
  mv -f -- "$state_publish" "$state_output"
  state_publish=
}
cleanup() {
  cleanup_status=$1
  trap - EXIT HUP INT TERM
  set +e
  if test "$verification_started" = true && test "$verification_passed" != true; then
    write_state failed "$cleanup_status" ""
  fi
  if test -n "$oci_publish"; then
    rm -f -- "$oci_publish"
  fi
  if test -n "$receipt_publish"; then
    rm -f -- "$receipt_publish"
  fi
  if test -n "$scan_publish"; then
    rm -f -- "$scan_publish"
  fi
  if test -n "$state_publish"; then
    rm -f -- "$state_publish"
  fi
  find "$work_dir" -depth -delete >/dev/null 2>&1
}
trap 'cleanup_status=$?; cleanup "$cleanup_status"' EXIT
trap 'trap - HUP; exit 129' HUP
trap 'trap - INT; exit 130' INT
trap 'trap - TERM; exit 143' TERM

mkdir -p -- "$(dirname -- "$oci_output")" "$(dirname -- "$receipt_output")" "$(dirname -- "$scan_output")" "$(dirname -- "$state_output")"
mkdir -p -- "$work_dir/context"
source_revision=$(git -C "$repo_root" rev-parse HEAD)
source_date_epoch=$(git -C "$repo_root" show -s --format=%ct "$source_revision")
gate_sha=$(sha256sum "$repo_root/hack/verify-runner-image.sh" | cut -d ' ' -f 1)
source_status=$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all)
source_status_sha=$(printf '%s' "$source_status" | sha256sum | cut -d ' ' -f 1)
source_dirty=false
if test -n "$source_status"; then
  source_dirty=true
fi
verification_started=true
tar -C "$repo_root" -cf "$work_dir/context.tar" -- \
  Dockerfile.runner Dockerfile.runner.dockerignore go.mod go.sum \
  cmd/oberth-runner internal/runner pkg/periapsis
tar -C "$work_dir/context" -xf "$work_dir/context.tar"
find "$work_dir/context" -type d -exec chmod 0755 {} +
find "$work_dir/context" -type f -perm -0100 -exec chmod 0755 {} +
find "$work_dir/context" -type f ! -perm -0100 -exec chmod 0644 {} +
if test "$(git -C "$repo_root" rev-parse HEAD)" != "$source_revision" || \
   test "$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all)" != "$source_status"; then
  echo "runner image gate: source changed while the context snapshot was created" >&2
  exit 1
fi
context_sha=$(context_tree_sha "$work_dir/context")
dockerfile_sha=$(sha256sum "$work_dir/context/Dockerfile.runner" | cut -d ' ' -f 1)
write_state running 0 ""
if test "$source_dirty" = true; then
  echo "runner image gate: refusing to authorize an image from a dirty source tree" >&2
  exit 1
fi

docker buildx build \
  --file "$work_dir/context/Dockerfile.runner" \
  --platform linux/amd64 \
  --provenance=false \
  --no-cache \
  --build-arg "SOURCE_DATE_EPOCH=$source_date_epoch" \
  --target runner \
  --output "type=oci,dest=$work_dir/runner.oci,rewrite-timestamp=true" \
  "$work_dir/context" >/dev/null
docker buildx build \
  --file "$work_dir/context/Dockerfile.runner" \
  --platform linux/amd64 \
  --provenance=false \
  --no-cache \
  --build-arg "SOURCE_DATE_EPOCH=$source_date_epoch" \
  --target runner \
  --output "type=oci,dest=$work_dir/runner.repro.oci,rewrite-timestamp=true" \
  "$work_dir/context" >/dev/null

trivy_cache=$work_dir/trivy-cache
metadata=$trivy_cache/db/metadata.json
mkdir -p "$trivy_cache"

verified_blob_path() {
  blob_layout=$1
  blob_digest=$2
  blob_size=$3
  if ! printf '%s\n' "$blob_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
     ! printf '%s\n' "$blob_size" | grep -Eq '^[0-9]+$'; then
    echo "runner image gate: invalid OCI descriptor" >&2
    return 1
  fi
  blob_path=$blob_layout/blobs/sha256/${blob_digest#sha256:}
  if test ! -f "$blob_path" || test "$(wc -c <"$blob_path" | tr -d ' ')" != "$blob_size" ||
     test "sha256:$(sha256sum "$blob_path" | cut -d ' ' -f 1)" != "$blob_digest"; then
    echo "runner image gate: OCI blob does not match descriptor $blob_digest" >&2
    return 1
  fi
  printf '%s\n' "$blob_path"
}

mkdir -p -- "$work_dir/oci-layout" "$work_dir/oci-repro-layout"
tar -xf "$work_dir/runner.oci" -C "$work_dir/oci-layout"
tar -xf "$work_dir/runner.repro.oci" -C "$work_dir/oci-repro-layout"
index_json=$(cat "$work_dir/oci-layout/index.json")
manifest_descriptor=$(printf '%s' "$index_json" | jq -cer '.manifests | if length == 1 then .[0] else error("expected one image manifest") end')
manifest_digest=$(printf '%s' "$manifest_descriptor" | jq -er '.digest')
manifest_size=$(printf '%s' "$manifest_descriptor" | jq -er '.size')
manifest_blob=$(verified_blob_path "$work_dir/oci-layout" "$manifest_digest" "$manifest_size")
repro_descriptor=$(jq -cer '.manifests | if length == 1 then .[0] else error("expected one image manifest") end' "$work_dir/oci-repro-layout/index.json")
repro_digest=$(printf '%s' "$repro_descriptor" | jq -er '.digest')
repro_size=$(printf '%s' "$repro_descriptor" | jq -er '.size')
verified_blob_path "$work_dir/oci-repro-layout" "$repro_digest" "$repro_size" >/dev/null
if test "$manifest_digest" != "$repro_digest"; then
  echo "runner image gate: repeated build produced a different manifest digest" >&2
  exit 1
fi
manifest_json=$(cat "$manifest_blob")
config_digest=$(printf '%s' "$manifest_json" | jq -er '.config.digest')
config_size=$(printf '%s' "$manifest_json" | jq -er '.config.size')
config_blob=$(verified_blob_path "$work_dir/oci-layout" "$config_digest" "$config_size")
layer_count=$(printf '%s' "$manifest_json" | jq -er '.layers | length')
if test "$layer_count" -lt 1; then
  echo "runner image gate: OCI manifest has no layers" >&2
  exit 1
fi
compressed_layer_bytes=$(printf '%s' "$manifest_json" | jq -er '[.layers[].size] | add')
if test "$compressed_layer_bytes" -le 0 || test "$compressed_layer_bytes" -gt 104857600; then
  echo "runner image gate: compressed layers are ${compressed_layer_bytes} bytes, limit is 104857600" >&2
  exit 1
fi
printf '%s' "$manifest_json" | jq -er '.layers[] | [.digest, (.size | tostring)] | @tsv' >"$work_dir/layers.tsv"
while IFS="$(printf '\t')" read -r layer_digest layer_size; do
  verified_blob_path "$work_dir/oci-layout" "$layer_digest" "$layer_size" >/dev/null
done <"$work_dir/layers.tsv"
config_json=$(cat "$config_blob")
platform=$(printf '%s' "$config_json" | jq -er '.os + "/" + .architecture')
image_user=$(printf '%s' "$config_json" | jq -er '.config.User')
image_created=$(printf '%s' "$config_json" | jq -er '.created')
expected_created=$(date -u -d "@$source_date_epoch" +%Y-%m-%dT%H:%M:%SZ)
test "$platform" = linux/amd64
test "$image_user" = 65534:65534
test "$image_created" = "$expected_created"

set +e
"$trivy_bin" image \
  --input "$work_dir/oci-layout" \
  --cache-dir "$trivy_cache" \
  --skip-check-update \
  --skip-vex-repo-update \
  --skip-version-check \
  --disable-telemetry \
  --scanners vuln,secret \
  --severity HIGH,CRITICAL \
  --list-all-pkgs=true \
  --skip-files '**/trivy.db' \
  --exit-code 1 \
  --format json \
  --output "$work_dir/scan.raw.json" \
  >"$work_dir/trivy.log" 2>&1
scan_status=$?
set -e

if test ! -s "$work_dir/scan.raw.json"; then
  tail -n 40 "$work_dir/trivy.log" >&2
  exit 1
fi
test -f "$metadata" || fail "Trivy scan did not materialize database metadata"
updated_at=$(jq -er '.UpdatedAt' "$metadata")
updated_epoch=$(utc_iso_to_epoch "$updated_at")
now_epoch=$(date -u +%s)
database_age=$((now_epoch - updated_epoch))
if test "$database_age" -lt -900 || test "$database_age" -gt 259200; then
  echo "runner image gate: Trivy database age is outside the allowed 72-hour window" >&2
  exit 1
fi
jq --arg digest "$manifest_digest" '.ArtifactName = $digest' \
  "$work_dir/scan.raw.json" >"$work_dir/scan.json"
if ! jq -e --arg image_id "$config_digest" \
  '.ArtifactType == "container_image" and .Metadata.ImageID == $image_id' \
  "$work_dir/scan.json" >/dev/null; then
  echo "runner image gate: Trivy report is not bound to the verified OCI config" >&2
  exit 1
fi

result_count=$(jq '[.Results[]?] | length' "$work_dir/scan.json")
package_count=$(jq '[.Results[]?.Packages[]?] | length' "$work_dir/scan.json")
os_target_count=$(jq '[.Results[]? | select(.Class == "os-pkgs" and .Type == "alpine" and ((.Packages // []) | length) > 0)] | length' "$work_dir/scan.json")
go_target_count=$(jq '[.Results[]? | select(.Class == "lang-pkgs" and .Type == "gobinary" and ((.Packages // []) | length) > 0)] | length' "$work_dir/scan.json")
expected_target=usr/local/bin/oberth-runner
if ! jq -e --arg target "$expected_target" \
  'any(.Results[]?; .Class == "lang-pkgs" and .Type == "gobinary" and
    (.Target | endswith($target)) and ((.Packages // []) | length) > 0)' \
  "$work_dir/scan.json" >/dev/null; then
  echo "runner image gate: scan omitted $expected_target" >&2
  exit 1
fi
for forbidden_target in golangci-lint cosign trivy helm etcd kube-apiserver kubectl; do
  if jq -e --arg target "$forbidden_target" \
    'any(.Results[]?; .Class == "lang-pkgs" and .Type == "gobinary" and
      (.Target | endswith($target)) and ((.Packages // []) | length) > 0)' \
    "$work_dir/scan.json" >/dev/null; then
    echo "runner image gate: forbidden baked tool found: $forbidden_target" >&2
    exit 1
  fi
done
for forbidden_package in python3; do
  if jq -e --arg package "$forbidden_package" \
    'any(.Results[]? | select(.Class == "os-pkgs") | .Packages[]?; .Name == $package)' \
    "$work_dir/scan.json" >/dev/null; then
    echo "runner image gate: forbidden baked package found: $forbidden_package" >&2
    exit 1
  fi
done
if jq -e 'any(.Results[]?; .Type == "python-pkg")' "$work_dir/scan.json" >/dev/null; then
  echo "runner image gate: forbidden Python package inventory found" >&2
  exit 1
fi
if test "$result_count" -lt 2 || test "$package_count" -lt 10 || test "$os_target_count" -lt 1 || test "$go_target_count" -ne 1; then
  echo "runner image gate: incomplete scan coverage results=$result_count packages=$package_count os=$os_target_count go=$go_target_count" >&2
  exit 1
fi

high=$(jq '[.Results[]?.Vulnerabilities[]? | select(.Severity == "HIGH")] | length' "$work_dir/scan.json")
critical=$(jq '[.Results[]?.Vulnerabilities[]? | select(.Severity == "CRITICAL")] | length' "$work_dir/scan.json")
secrets=$(jq '[.Results[]?.Secrets[]?] | length' "$work_dir/scan.json")
scan_sha=$(sha256sum "$work_dir/scan.json" | cut -d ' ' -f 1)
archive_sha=$(sha256sum "$work_dir/runner.oci" | cut -d ' ' -f 1)
database_sha=$(sha256sum "$trivy_cache/db/trivy.db" | cut -d ' ' -f 1)
metadata_sha=$(sha256sum "$metadata" | cut -d ' ' -f 1)
trivy_version=$($trivy_bin --version | sed -n 's/^Version: //p')

jq -n \
  --arg manifest "$manifest_digest" \
  --arg config_digest "$config_digest" \
  --arg archive_sha256 "$archive_sha" \
  --arg dockerfile_sha256 "$dockerfile_sha" \
  --arg context_sha256 "$context_sha" \
  --arg source_revision "$source_revision" \
  --arg source_status_sha256 "$source_status_sha" \
  --arg gate_sha256 "$gate_sha" \
  --arg source_date_epoch "$source_date_epoch" \
  --arg platform "$platform" \
  --arg user "$image_user" \
  --arg created "$image_created" \
  --arg trivy_version "$trivy_version" \
  --arg database_sha256 "$database_sha" \
  --arg metadata_sha256 "$metadata_sha" \
  --arg updated_at "$updated_at" \
  --arg next_update "$(jq -er '.NextUpdate' "$metadata")" \
  --arg scan_sha256 "$scan_sha" \
  --arg scan_file "$(basename -- "$scan_output")" \
  --argjson high "$high" \
  --argjson critical "$critical" \
  --argjson secrets "$secrets" \
  --argjson scanner_exit_code "$scan_status" \
  --argjson source_dirty "$source_dirty" \
  --argjson result_count "$result_count" \
  --argjson package_count "$package_count" \
  --argjson os_target_count "$os_target_count" \
  --argjson go_target_count "$go_target_count" \
  --argjson layer_count "$layer_count" \
  --argjson compressed_layer_bytes "$compressed_layer_bytes" \
  '{
    schema: 1,
    passed: true,
    source: {
      revision: $source_revision,
      contextSHA256: $context_sha256,
      sourceDateEpoch: $source_date_epoch,
      dirty: $source_dirty,
      statusSHA256: $source_status_sha256,
      gateSHA256: $gate_sha256
    },
    image: {
      manifest: $manifest,
      configDigest: $config_digest,
      layerCount: $layer_count,
      compressedLayerBytes: $compressed_layer_bytes,
      archiveSHA256: $archive_sha256,
      dockerfileSHA256: $dockerfile_sha256,
      platform: $platform,
      user: $user,
      created: $created
    },
    scanner: {
      trivyVersion: $trivy_version,
      databaseSHA256: $database_sha256,
      metadataSHA256: $metadata_sha256,
      databaseUpdatedAt: $updated_at,
      databaseNextUpdate: $next_update,
      reportSHA256: $scan_sha256,
      reportFile: $scan_file,
      exitCode: $scanner_exit_code,
      resultCount: $result_count,
      packageCount: $package_count,
      osTargetCount: $os_target_count,
      goBinaryTargetCount: $go_target_count,
      high: $high,
      critical: $critical,
      secrets: $secrets
    }
  }' >"$work_dir/receipt.json"

if test "$scan_status" -ne 0 || test "$high" -ne 0 || test "$critical" -ne 0 || test "$secrets" -ne 0; then
  tail -n 40 "$work_dir/trivy.log" >&2
  echo "runner image gate: high=$high critical=$critical secrets=$secrets" >&2
  exit 1
fi
oci_publish=$oci_output.tmp.$$
receipt_publish=$receipt_output.tmp.$$
scan_publish=$scan_output.tmp.$$
install -m 0644 "$work_dir/runner.oci" "$oci_publish"
install -m 0644 "$work_dir/receipt.json" "$receipt_publish"
install -m 0644 "$work_dir/scan.json" "$scan_publish"
mv -f -- "$oci_publish" "$oci_output"
mv -f -- "$scan_publish" "$scan_output"
mv -f -- "$receipt_publish" "$receipt_output"

published_manifest=$(tar -xOf "$oci_output" index.json | jq -er '.manifests | if length == 1 then .[0].digest else error("expected one image manifest") end')
published_archive_sha=$(sha256sum "$oci_output" | cut -d ' ' -f 1)
published_scan_sha=$(sha256sum "$scan_output" | cut -d ' ' -f 1)
if ! jq -e \
  --arg manifest "$published_manifest" \
  --arg archive_sha "$published_archive_sha" \
  --arg scan_sha "$published_scan_sha" \
  '.passed == true and .scanner.exitCode == 0 and
   .image.manifest == $manifest and .image.archiveSHA256 == $archive_sha and
   .scanner.reportSHA256 == $scan_sha' \
  "$receipt_output" >/dev/null; then
  echo "runner image gate: published OCI, scan, and receipt do not form one verified generation" >&2
  exit 1
fi
published_receipt_sha=$(sha256sum "$receipt_output" | cut -d ' ' -f 1)
write_state passed 0 "$published_receipt_sha"
verification_passed=true
