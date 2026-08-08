#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
values=${1:-"$repo_root/charts/oberth/values.yaml"}
oci=${2:-"$repo_root/dist/oberth-runner.oci"}
receipt=${3:-"$repo_root/artifacts/runner-image-receipt.json"}
scan=${4:-"$repo_root/artifacts/runner-image-scan.json"}
state=${5:-"$repo_root/artifacts/runner-image-state.json"}
publication=${6:-"$repo_root/artifacts/runner-image-publication.json"}

repository=europe-west4-docker.pkg.dev/skipopsmain/cloudtaser/cloudtaser-oberth-ci
verify_tmpdir=${RUNNER_VERIFY_TMPDIR:-/var/tmp}
registry_timeout=${RUNNER_REGISTRY_TIMEOUT:-60s}
registry_pull_timeout=${RUNNER_REGISTRY_PULL_TIMEOUT:-30m}
crane_source=$repo_root/bin/crane
crane_pin=$repo_root/hack/runner-crane.sha256
publisher_contract=$repo_root/hack/publish-runner-image.sh
oci_helper=$repo_root/hack/runner-oci.py

fail() {
  printf 'runner pin gate: %s\n' "$*" >&2
  exit 1
}

for command_name in flock jq sha256sum timeout grep cut mktemp tr wc find cp chmod mkdir python3 stat id; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done
case "$registry_timeout" in
  1m|2m) ;;
  *s)
    printf '%s\n' "$registry_timeout" | grep -Eq '^([1-9]|[1-9][0-9]|1[01][0-9]|120)s$' || \
      fail "RUNNER_REGISTRY_TIMEOUT must be between 1s and 120s"
    ;;
  *) fail "RUNNER_REGISTRY_TIMEOUT must be between 1s and 120s" ;;
esac
case "$registry_pull_timeout" in
  *s)
    registry_pull_seconds=${registry_pull_timeout%s}
    printf '%s\n' "$registry_pull_seconds" | grep -Eq '^[1-9][0-9]{0,3}$' || \
      fail "RUNNER_REGISTRY_PULL_TIMEOUT must be between 1s and 30m"
    test "$registry_pull_seconds" -le 1800 || \
      fail "RUNNER_REGISTRY_PULL_TIMEOUT must be between 1s and 30m"
    ;;
  *m)
    registry_pull_minutes=${registry_pull_timeout%m}
    printf '%s\n' "$registry_pull_minutes" | grep -Eq '^[1-9][0-9]?$' || \
      fail "RUNNER_REGISTRY_PULL_TIMEOUT must be between 1s and 30m"
    test "$registry_pull_minutes" -le 30 || \
      fail "RUNNER_REGISTRY_PULL_TIMEOUT must be between 1s and 30m"
    ;;
  *) fail "RUNNER_REGISTRY_PULL_TIMEOUT must be between 1s and 30m" ;;
esac
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

publication_dir=$(dirname -- "$publication")
if test ! -d "$publication_dir" || test -L "$publication_dir"; then
  fail "publication evidence directory is not a regular directory: $publication_dir"
fi
publication_lock=$lock_dir/publication.lock
test ! -L "$publication_lock" || fail "publication lock must not be a symlink"
if test -e "$publication_lock" && test ! -f "$publication_lock"; then
  fail "publication lock must be a regular file"
fi
exec 9>"$publication_lock" || fail "cannot open publication lock: $publication_lock"
flock -s -n 9 || fail "runner image publication is still in progress"

evidence_lock=$lock_dir/evidence.lock
test ! -L "$evidence_lock" || fail "runner evidence lock must not be a symlink"
if test -e "$evidence_lock" && test ! -f "$evidence_lock"; then
  fail "runner evidence lock must be a regular file"
fi
exec 8>"$evidence_lock" || fail "cannot open runner evidence lock: $evidence_lock"
flock -s -n 8 || fail "runner image verification is still in progress"

"$repo_root/hack/verify-runner-evidence.sh" "$values" "$oci" "$receipt" "$scan" "$state" || \
  fail "local runner evidence is not deployable"

for required in "$publication" "$crane_source" "$crane_pin" "$publisher_contract" "$oci_helper"; do
  if test ! -f "$required" || test -L "$required"; then
    fail "required publication input is not a regular file: $required"
  fi
done
test -x "$crane_source" || fail "source-owned crane is not executable: $crane_source"
test "$(wc -l <"$crane_pin" | tr -d ' ')" = 1 || fail "source-owned crane checksum must contain exactly one entry"
grep -Eq '^[0-9a-f]{64}  bin/crane$' "$crane_pin" || fail "source-owned crane checksum has an invalid format"
crane_sha=$(cut -d ' ' -f 1 "$crane_pin")
test "$(sha256sum "$crane_source" | cut -d ' ' -f 1)" = "$crane_sha" || \
  fail "crane executable does not match the source-owned checksum"
publisher_contract_sha=$(sha256sum "$publisher_contract" | cut -d ' ' -f 1)
oci_helper_sha=$(sha256sum "$oci_helper" | cut -d ' ' -f 1)

work_dir=$(mktemp -d "$verify_tmpdir/oberth-runner-pin.XXXXXX") || fail "cannot create verification work directory"
chmod 0700 "$work_dir"
cleanup() {
  cleanup_status=$?
  trap - EXIT HUP INT TERM
  find "$work_dir" -depth -delete >/dev/null 2>&1 || cleanup_status=1
  exit "$cleanup_status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -m 0700 "$work_dir/bin"
cp -- "$crane_source" "$work_dir/bin/crane"
chmod 0500 "$work_dir/bin/crane"
trusted_crane=$work_dir/bin/crane
test "$(sha256sum "$trusted_crane" | cut -d ' ' -f 1)" = "$crane_sha" || \
  fail "private crane copy failed checksum verification"
cp -- "$oci_helper" "$work_dir/bin/runner-oci.py"
chmod 0500 "$work_dir/bin/runner-oci.py"
trusted_oci_helper=$work_dir/bin/runner-oci.py
test "$(sha256sum "$trusted_oci_helper" | cut -d ' ' -f 1)" = "$oci_helper_sha" || \
  fail "private OCI helper copy failed checksum verification"
mkdir -m 0700 "$work_dir/docker-config"
printf '%s' '{"auths":{}}' >"$work_dir/docker-config/config.json"
chmod 0600 "$work_dir/docker-config/config.json"
DOCKER_CONFIG=$work_dir/docker-config
export DOCKER_CONFIG
unset REGISTRY_AUTH_FILE XDG_RUNTIME_DIR

crane_version=$("$trusted_crane" version 2>/dev/null) || fail "trusted crane version check failed"
test "$crane_version" = 0.20.3 || fail "trusted crane version is $crane_version, expected 0.20.3"

receipt_sha=$(sha256sum "$receipt" | cut -d ' ' -f 1)
state_sha=$(sha256sum "$state" | cut -d ' ' -f 1)
archive_sha=$(sha256sum "$oci" | cut -d ' ' -f 1)
scan_sha=$(sha256sum "$scan" | cut -d ' ' -f 1)
values_sha=$(sha256sum "$values" | cut -d ' ' -f 1)
source_revision=$(jq -er '.source.revision' "$receipt")
source_context_sha=$(jq -er '.source.contextSHA256' "$receipt")
manifest_digest=$(jq -er '.image.manifest' "$receipt")
config_digest=$(jq -er '.image.configDigest' "$receipt")
layer_count=$(jq -er '.image.layerCount' "$receipt")
printf '%s\n' "$manifest_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || fail "receipt manifest digest is invalid"
printf '%s\n' "$config_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || fail "receipt config digest is invalid"
printf '%s\n' "$layer_count" | grep -Eq '^[1-9][0-9]*$' || fail "receipt layer count is invalid"

tag=runner-${manifest_digest#sha256:}
tag_ref=$repository:$tag
digest_ref=$repository@$manifest_digest
if ! jq -e \
  --arg source_revision "$source_revision" \
  --arg source_context_sha "$source_context_sha" \
  --arg receipt_sha "$receipt_sha" \
  --arg state_sha "$state_sha" \
  --arg archive_sha "$archive_sha" \
  --arg scan_sha "$scan_sha" \
  --arg values_sha "$values_sha" \
  --arg repository "$repository" \
  --arg tag "$tag" \
  --arg tag_ref "$tag_ref" \
  --arg digest_ref "$digest_ref" \
  --arg manifest "$manifest_digest" \
  --arg config_digest "$config_digest" \
  --argjson layer_count "$layer_count" \
  --arg crane_sha "$crane_sha" \
  --arg crane_version "$crane_version" \
  --arg publisher_contract_sha "$publisher_contract_sha" \
  --arg oci_helper_sha "$oci_helper_sha" '
    .schema == 1 and .status == "passed" and .exitCode == 0 and
    .source.revision == $source_revision and
    .source.contextSHA256 == $source_context_sha and
    .source.receiptSHA256 == $receipt_sha and
    .source.stateSHA256 == $state_sha and
    .source.ociArchiveSHA256 == $archive_sha and
    .source.scanSHA256 == $scan_sha and
    .source.valuesSHA256 == $values_sha and
    .destination.repository == $repository and
    .destination.tag == $tag and
    .destination.tagRef == $tag_ref and
    .destination.digestRef == $digest_ref and
    .destination.manifest == $manifest and
    (.result.pushed | type) == "boolean" and
    (.result.reconciled | type) == "boolean" and
    (.result.attempts | type) == "number" and
    .result.attempts >= 0 and .result.attempts <= 3 and
    .publisher.craneSHA256 == $crane_sha and
    .publisher.craneVersion == $crane_version and
    .publisher.contractSHA256 == $publisher_contract_sha and
    .publisher.ociHelperSHA256 == $oci_helper_sha and
    .readBack.tagManifest == $manifest and
    .readBack.digestManifest == $manifest and
    .readBack.configDigest == $config_digest and
    .readBack.layerCount == $layer_count and
    (.readBack.tagOCILayoutSHA256 | test("^[0-9a-f]{64}$")) and
    (.readBack.digestOCILayoutSHA256 | test("^[0-9a-f]{64}$"))
  ' "$publication" >/dev/null; then
  fail "latest publication attempt is not passing evidence for the current runner pin"
fi

published_tag_layout_sha=$(jq -er '.readBack.tagOCILayoutSHA256' "$publication")
published_digest_layout_sha=$(jq -er '.readBack.digestOCILayoutSHA256' "$publication")

registry_digest() {
  registry_ref=$1
  output_file=$2
  error_file=$3
  : >"$output_file"
  : >"$error_file"
  if ! timeout --foreground --signal=TERM --kill-after=10s "$registry_timeout" \
    "$trusted_crane" digest "$registry_ref" >"$output_file" 2>"$error_file"; then
    registry_error=$(tr '\n' ' ' <"$error_file" | cut -c 1-1000)
    test -n "$registry_error" || registry_error="registry lookup failed"
    fail "$registry_error"
  fi
  resolved_digest=$(tr -d '\r\n ' <"$output_file")
  printf '%s\n' "$resolved_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || \
    fail "registry returned an invalid digest for $registry_ref"
  printf '%s\n' "$resolved_digest"
}

live_tag_digest=$(registry_digest "$tag_ref" "$work_dir/tag.stdout" "$work_dir/tag.stderr")
test "$live_tag_digest" = "$manifest_digest" || \
  fail "live immutable runner tag resolves to $live_tag_digest, expected $manifest_digest"
live_digest=$(registry_digest "$digest_ref" "$work_dir/digest.stdout" "$work_dir/digest.stderr")
test "$live_digest" = "$manifest_digest" || \
  fail "live runner digest reference resolves to $live_digest, expected $manifest_digest"

pull_live_oci() {
  live_ref=$1
  live_layout=$2
  live_label=$3
  live_expected_layout_sha=$4
  live_prefix=$5
  if ! timeout --foreground --signal=TERM --kill-after=10s "$registry_pull_timeout" \
    "$trusted_crane" pull --format=oci "$live_ref" "$live_layout" \
    >"$live_prefix.stdout" 2>"$live_prefix.stderr"; then
    live_error=$(tr '\n' ' ' <"$live_prefix.stderr" | cut -c 1-1000)
    test -n "$live_error" || live_error="registry OCI pull failed"
    fail "$live_label pull failed: $live_error"
  fi
  if test ! -d "$live_layout" || test -L "$live_layout"; then
    fail "$live_label pull did not create a regular OCI layout directory"
  fi
  if ! python3 -I -S -B "$trusted_oci_helper" validate "$live_layout" "$manifest_digest" "$live_label" \
    >"$live_prefix.json"; then
    fail "$live_label validation failed"
  fi
  live_config_digest=$(jq -er '.configDigest' "$live_prefix.json")
  live_layer_count=$(jq -er '.layerCount' "$live_prefix.json")
  live_layout_sha=$(jq -er '.layoutSHA256' "$live_prefix.json")
  test "$live_config_digest" = "$config_digest" || \
    fail "$live_label config digest differs from the verified publication"
  test "$live_layer_count" = "$layer_count" || \
    fail "$live_label layer count differs from the verified publication"
  test "$live_layout_sha" = "$live_expected_layout_sha" || \
    fail "$live_label layout differs from the verified publication"
}

pull_live_oci "$tag_ref" "$work_dir/live-tag-layout" "live tag OCI graph" \
  "$published_tag_layout_sha" "$work_dir/live-tag"
pull_live_oci "$digest_ref" "$work_dir/live-digest-layout" "live digest OCI graph" \
  "$published_digest_layout_sha" "$work_dir/live-digest"

printf 'runner pin gate: verified current evidence, publication, and complete live OCI graphs for %s\n' "$digest_ref"
