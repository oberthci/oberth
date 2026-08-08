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
registry_host=europe-west4-docker.pkg.dev
publish_tmpdir=${RUNNER_PUBLISH_TMPDIR:-/var/tmp}
verify_tmpdir=${RUNNER_VERIFY_TMPDIR:-/var/tmp}
max_attempts=${RUNNER_PUBLISH_MAX_ATTEMPTS:-3}
retry_delay=${RUNNER_PUBLISH_RETRY_DELAY:-2}
registry_timeout=${RUNNER_REGISTRY_TIMEOUT:-60s}
registry_upload_timeout=${RUNNER_REGISTRY_UPLOAD_TIMEOUT:-30m}
registry_pull_timeout=${RUNNER_REGISTRY_PULL_TIMEOUT:-30m}
crane_source=$repo_root/bin/crane
crane_pin=$repo_root/hack/runner-crane.sha256
publisher_contract=$repo_root/hack/publish-runner-image.sh
oci_helper=$repo_root/hack/runner-oci.py

publication_dir=$(dirname -- "$publication")
mkdir -p -- "$publication_dir"
if test ! -d "$publication_dir" || test -L "$publication_dir"; then
  printf 'runner image publication: publication evidence directory is not a regular directory: %s\n' "$publication_dir" >&2
  exit 1
fi
umask 077

attempt_id=$(date -u +%Y%m%dT%H%M%SZ)-$$
started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
completed_at=
publication_error=
work_dir=
finished=false
lock_acquired=false
pushed=false
reconciled=false
publish_attempts=0
source_revision=
source_context_sha=
receipt_sha=
state_sha=
archive_sha=
scan_sha=
values_sha=
manifest_digest=
config_digest=
layer_count=0
tag=
tag_ref=
digest_ref=
readback_tag_manifest=
readback_digest_manifest=
readback_config_digest=
readback_layer_count=0
readback_tag_layout_sha=
readback_digest_layout_sha=
trusted_crane=
trusted_crane_sha=
trusted_crane_version=
trusted_oci_helper=
publisher_contract_sha=
oci_helper_sha=

write_fallback_failure() {
  fallback_tmp=$publication_dir/.runner-image-publication.$$.tmp
  umask 077
  printf '%s\n' '{"schema":1,"status":"failed","exitCode":1,"error":"publication state writer is unavailable"}' >"$fallback_tmp" || return 1
  chmod 0644 "$fallback_tmp" || return 1
  mv -f -- "$fallback_tmp" "$publication"
}

write_publication() {
  publication_status=$1
  publication_exit_code=$2
  publication_tmp=$(mktemp "$publication_dir/.runner-image-publication.XXXXXX") || return 1
  if ! jq -n \
    --arg status "$publication_status" \
    --argjson exit_code "$publication_exit_code" \
    --arg attempt_id "$attempt_id" \
    --arg started_at "$started_at" \
    --arg completed_at "$completed_at" \
    --arg error "$publication_error" \
    --arg source_revision "$source_revision" \
    --arg source_context_sha256 "$source_context_sha" \
    --arg receipt_sha256 "$receipt_sha" \
    --arg state_sha256 "$state_sha" \
    --arg oci_archive_sha256 "$archive_sha" \
    --arg scan_sha256 "$scan_sha" \
    --arg values_sha256 "$values_sha" \
    --arg repository "$repository" \
    --arg tag "$tag" \
    --arg tag_ref "$tag_ref" \
    --arg digest_ref "$digest_ref" \
    --arg manifest "$manifest_digest" \
    --argjson pushed "$pushed" \
    --argjson reconciled "$reconciled" \
    --argjson attempts "$publish_attempts" \
    --arg tag_manifest "$readback_tag_manifest" \
    --arg digest_manifest "$readback_digest_manifest" \
    --arg config_digest "$readback_config_digest" \
    --argjson layer_count "$readback_layer_count" \
    --arg tag_layout_sha256 "$readback_tag_layout_sha" \
    --arg digest_layout_sha256 "$readback_digest_layout_sha" \
    --arg crane_sha256 "$trusted_crane_sha" \
    --arg crane_version "$trusted_crane_version" \
    --arg contract_sha256 "$publisher_contract_sha" \
    --arg oci_helper_sha256 "$oci_helper_sha" \
    '{
      schema: 1,
      status: $status,
      exitCode: $exit_code,
      attemptID: $attempt_id,
      startedAt: $started_at,
      completedAt: $completed_at,
      error: $error,
      source: {
        revision: $source_revision,
        contextSHA256: $source_context_sha256,
        receiptSHA256: $receipt_sha256,
        stateSHA256: $state_sha256,
        ociArchiveSHA256: $oci_archive_sha256,
        scanSHA256: $scan_sha256,
        valuesSHA256: $values_sha256
      },
      destination: {
        repository: $repository,
        tag: $tag,
        tagRef: $tag_ref,
        digestRef: $digest_ref,
        manifest: $manifest
      },
      result: {
        pushed: $pushed,
        reconciled: $reconciled,
        attempts: $attempts
      },
      publisher: {
        craneVersion: $crane_version,
        craneSHA256: $crane_sha256,
        contractSHA256: $contract_sha256,
        ociHelperSHA256: $oci_helper_sha256
      },
      readBack: {
        tagManifest: $tag_manifest,
        digestManifest: $digest_manifest,
        configDigest: $config_digest,
        layerCount: $layer_count,
        tagOCILayoutSHA256: $tag_layout_sha256,
        digestOCILayoutSHA256: $digest_layout_sha256
      }
    }' >"$publication_tmp"; then
    rm -f -- "$publication_tmp"
    return 1
  fi
  chmod 0644 "$publication_tmp" || {
    rm -f -- "$publication_tmp"
    return 1
  }
  mv -f -- "$publication_tmp" "$publication"
}

cleanup() {
  cleanup_status=$?
  trap - EXIT HUP INT TERM
  set +e
  if test -n "$work_dir" && test -d "$work_dir"; then
    rm -rf -- "$work_dir"
    if test -e "$work_dir"; then
      publication_error='failed to remove ephemeral publication credentials and work data'
      cleanup_status=1
    fi
  fi
  if test "$lock_acquired" = true &&
     { test "$finished" != true || test "$cleanup_status" -ne 0; }; then
    test "$cleanup_status" -ne 0 || cleanup_status=1
    completed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || true)
    write_publication failed "$cleanup_status" || write_fallback_failure || true
  fi
  exit "$cleanup_status"
}
trap cleanup EXIT
trap 'publication_error="publication interrupted by HUP"; exit 129' HUP
trap 'publication_error="publication interrupted by INT"; exit 130' INT
trap 'publication_error="publication interrupted by TERM"; exit 143' TERM

fail() {
  publication_error=$*
  printf 'runner image publication: %s\n' "$*" >&2
  exit 1
}

for command_name in flock sha256sum stat id mkdir; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done
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
publication_lock=$lock_dir/publication.lock
test ! -L "$publication_lock" || fail "publication lock must not be a symlink"
if test -e "$publication_lock" && test ! -f "$publication_lock"; then
  fail "publication lock must be a regular file"
fi
exec 9>"$publication_lock" || fail "cannot open publication attempt lock: $publication_lock"
flock -x -n 9 || fail "another runner publication attempt holds $publication_lock"
lock_acquired=true

if ! command -v jq >/dev/null 2>&1; then
  publication_error='jq is required to write publication state'
  write_fallback_failure || true
  fail "$publication_error"
fi
write_publication running 0 || fail "cannot write running publication state to $publication"

for command_name in python3 sha256sum timeout awk grep cp mktemp sed tr sleep cut wc find; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done
for contract_file in "$publisher_contract" "$oci_helper"; do
  if test ! -f "$contract_file" || test -L "$contract_file"; then
    fail "publisher contract input is not a regular file: $contract_file"
  fi
done
publisher_contract_sha=$(sha256sum "$publisher_contract" | cut -d ' ' -f 1)
oci_helper_sha=$(sha256sum "$oci_helper" | cut -d ' ' -f 1)
printf '%s\n' "$max_attempts" | grep -Eq '^[1-3]$' || fail "RUNNER_PUBLISH_MAX_ATTEMPTS must be between 1 and 3"
printf '%s\n' "$retry_delay" | grep -Eq '^[0-9]+$' || fail "RUNNER_PUBLISH_RETRY_DELAY must be an integer"
test "$retry_delay" -le 60 || fail "RUNNER_PUBLISH_RETRY_DELAY must be at most 60 seconds"
case "$registry_timeout" in
  1m|2m) ;;
  *s)
    printf '%s\n' "$registry_timeout" | grep -Eq '^([1-9]|[1-9][0-9]|1[01][0-9]|120)s$' || \
      fail "RUNNER_REGISTRY_TIMEOUT must be between 1s and 120s"
    ;;
  *) fail "RUNNER_REGISTRY_TIMEOUT must be between 1s and 120s" ;;
esac
case "$registry_upload_timeout" in
  *s)
    registry_upload_seconds=${registry_upload_timeout%s}
    printf '%s\n' "$registry_upload_seconds" | grep -Eq '^[1-9][0-9]{0,3}$' || \
      fail "RUNNER_REGISTRY_UPLOAD_TIMEOUT must be between 1s and 30m"
    test "$registry_upload_seconds" -le 1800 || \
      fail "RUNNER_REGISTRY_UPLOAD_TIMEOUT must be between 1s and 30m"
    ;;
  *m)
    registry_upload_minutes=${registry_upload_timeout%m}
    printf '%s\n' "$registry_upload_minutes" | grep -Eq '^[1-9][0-9]?$' || \
      fail "RUNNER_REGISTRY_UPLOAD_TIMEOUT must be between 1s and 30m"
    test "$registry_upload_minutes" -le 30 || \
      fail "RUNNER_REGISTRY_UPLOAD_TIMEOUT must be between 1s and 30m"
    ;;
  *) fail "RUNNER_REGISTRY_UPLOAD_TIMEOUT must be between 1s and 30m" ;;
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
test -d "$publish_tmpdir" || fail "publication temporary directory does not exist: $publish_tmpdir"
test -d "$verify_tmpdir" || fail "verification lock directory does not exist: $verify_tmpdir"

work_dir=$(mktemp -d "$publish_tmpdir/runner-image-publish.XXXXXX") || fail "cannot create publication work directory"
chmod 0700 "$work_dir"

evidence_lock=$lock_dir/evidence.lock
test ! -L "$evidence_lock" || fail "runner evidence lock must not be a symlink"
if test -e "$evidence_lock" && test ! -f "$evidence_lock"; then
  fail "runner evidence lock must be a regular file"
fi
exec 8>"$evidence_lock" || fail "cannot open runner evidence lock: $evidence_lock"
flock -s -n 8 || fail "runner image evidence generation is still in progress"

if test ! -f "$crane_source" || test -L "$crane_source" || test ! -x "$crane_source"; then
  fail "trusted crane input must be a regular executable: $crane_source"
fi
if test ! -f "$crane_pin" || test -L "$crane_pin"; then
  fail "source-owned crane checksum is missing: $crane_pin"
fi
test "$(wc -l <"$crane_pin" | tr -d ' ')" = 1 || fail "source-owned crane checksum must contain exactly one entry"
grep -Eq '^[0-9a-f]{64}  bin/crane$' "$crane_pin" || fail "source-owned crane checksum has an invalid format"
trusted_crane_sha=$(cut -d ' ' -f 1 "$crane_pin")
test "$(sha256sum "$crane_source" | cut -d ' ' -f 1)" = "$trusted_crane_sha" || \
  fail "crane executable does not match the source-owned checksum"
mkdir -m 0700 -- "$work_dir/bin"
cp -- "$crane_source" "$work_dir/bin/crane"
chmod 0500 "$work_dir/bin/crane"
trusted_crane=$work_dir/bin/crane
test "$(sha256sum "$trusted_crane" | cut -d ' ' -f 1)" = "$trusted_crane_sha" || \
  fail "private crane copy failed checksum verification"
cp -- "$oci_helper" "$work_dir/bin/runner-oci.py"
chmod 0500 "$work_dir/bin/runner-oci.py"
trusted_oci_helper=$work_dir/bin/runner-oci.py
test "$(sha256sum "$trusted_oci_helper" | cut -d ' ' -f 1)" = "$oci_helper_sha" || \
  fail "private OCI helper copy failed checksum verification"
mkdir -m 0700 -- "$work_dir/no-docker-config"
trusted_crane_version=$(DOCKER_CONFIG=$work_dir/no-docker-config "$trusted_crane" version 2>/dev/null) || \
  fail "trusted crane version check failed"
test "$trusted_crane_version" = 0.20.3 || fail "trusted crane version is $trusted_crane_version, expected 0.20.3"

for required in "$values" "$oci" "$receipt" "$scan" "$state"; do
  if test ! -f "$required" || test -L "$required"; then
    fail "required local evidence is not a regular file: $required"
  fi
done

extract_oci_archive() {
  python3 -I -S -B "$trusted_oci_helper" extract "$1" "$2" "$3"
}

validate_oci_layout() {
  python3 -I -S -B "$trusted_oci_helper" validate "$1" "$2" "$3" >"$4"
}
if ! extract_oci_archive "$oci" "$work_dir/local-layout" 'local OCI'; then
  fail "local OCI archive extraction failed"
fi
if ! validate_oci_layout "$work_dir/local-layout" - 'local OCI' "$work_dir/local-summary.json"; then
  fail "local OCI graph validation failed"
fi
manifest_digest=$(jq -er '.manifest' "$work_dir/local-summary.json")
config_digest=$(jq -er '.configDigest' "$work_dir/local-summary.json")
layer_count=$(jq -er '.layerCount' "$work_dir/local-summary.json")
printf '%s\n' "$manifest_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || fail "local OCI manifest digest is invalid"

archive_sha=$(sha256sum "$oci" | cut -d ' ' -f 1)
receipt_sha=$(sha256sum "$receipt" | cut -d ' ' -f 1)
state_sha=$(sha256sum "$state" | cut -d ' ' -f 1)
scan_sha=$(sha256sum "$scan" | cut -d ' ' -f 1)
values_sha=$(sha256sum "$values" | cut -d ' ' -f 1)
if ! jq -e --arg receipt_sha "$receipt_sha" \
  '.schema == 1 and .status == "passed" and .exitCode == 0 and .receiptSHA256 == $receipt_sha' \
  "$state" >/dev/null; then
  fail "attempt state does not identify the passing receipt"
fi
if ! jq -e \
  '.schema == 1 and .passed == true and .source.dirty == false and
   .scanner.exitCode == 0 and .scanner.high == 0 and
   .scanner.critical == 0 and .scanner.secrets == 0' "$receipt" >/dev/null; then
  fail "runner receipt is not clean and passing"
fi
source_revision=$(jq -er '.source.revision | select(type == "string" and length > 0)' "$receipt")
source_context_sha=$(jq -er '.source.contextSHA256 | select(type == "string" and test("^[0-9a-f]{64}$"))' "$receipt")
test "$source_revision" = "$(jq -er '.sourceRevision' "$state")" || \
  fail "attempt state and receipt describe different source revisions"
test "$source_context_sha" = "$(jq -er '.sourceContextSHA256' "$state")" || \
  fail "attempt state and receipt describe different source contexts"
test "$archive_sha" = "$(jq -er '.image.archiveSHA256' "$receipt")" || \
  fail "OCI archive hash does not match the receipt"
test "$manifest_digest" = "$(jq -er '.image.manifest' "$receipt")" || \
  fail "OCI manifest does not match the receipt"

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
expected_runner_ref=$repository@$manifest_digest
test "$runner_ref" = "$expected_runner_ref" || \
  fail "chart runnerImage.ref must be $expected_runner_ref"

"$repo_root/hack/verify-runner-evidence.sh" "$values" "$oci" "$receipt" "$scan" "$state" || \
  fail "source-owned runner evidence gate rejected the publication inputs"

test "${DOCKER_CONFIG+x}" = x || fail "DOCKER_CONFIG must name a caller-supplied ephemeral credential directory"
caller_docker_config=$DOCKER_CONFIG
if test -z "$caller_docker_config" || test ! -d "$caller_docker_config"; then
  fail "DOCKER_CONFIG must name a caller-supplied ephemeral credential directory"
fi
caller_config=$caller_docker_config/config.json
if test ! -f "$caller_config" || test -L "$caller_config"; then
  fail "DOCKER_CONFIG/config.json must be a regular file"
fi
mkdir -m 0700 -- "$work_dir/docker-config"
if ! jq -e --arg registry "$registry_host" '
  if (((.credsStore // "") == "") and
      ((.credHelpers[$registry] // "") == "") and
      (.auths[$registry] | type == "object") and
      (((.auths[$registry].auth // "") | type == "string" and length > 0) or
       (((.auths[$registry].username // "") | type == "string" and length > 0) and
        ((.auths[$registry].password // "") | type == "string" and length > 0))))
  then {auths: {($registry): .auths[$registry]}}
  else error("direct target registry auth is required")
  end
' "$caller_config" >"$work_dir/docker-config/config.json"; then
  rm -f -- "$work_dir/docker-config/config.json"
  fail "DOCKER_CONFIG must contain direct $registry_host auth and no credential helper"
fi
chmod 0600 "$work_dir/docker-config/config.json"
DOCKER_CONFIG=$work_dir/docker-config
export DOCKER_CONFIG
unset REGISTRY_AUTH_FILE XDG_RUNTIME_DIR

tag=runner-${manifest_digest#sha256:}
tag_ref=$repository:$tag
digest_ref=$repository@$manifest_digest

registry_stdout=$work_dir/registry.stdout
registry_stderr=$work_dir/registry.stderr
last_registry_error=
registry_call() {
  registry_call_timeout=$1
  shift
  registry_status=0
  : >"$registry_stdout"
  : >"$registry_stderr"
  if timeout --foreground --signal=TERM --kill-after=10s "$registry_call_timeout" \
    "$trusted_crane" "$@" >"$registry_stdout" 2>"$registry_stderr"; then
    return 0
  else
    registry_status=$?
  fi
  last_registry_error=$(tr '\n' ' ' <"$registry_stderr" | sed 's/[[:space:]][[:space:]]*/ /g; s/[[:space:]]$//' | cut -c 1-1000)
  if test -z "$last_registry_error"; then
    if test "$registry_status" -eq 124; then
      last_registry_error="registry command timed out after $registry_call_timeout"
    else
      last_registry_error="registry command exited $registry_status"
    fi
  fi
  return "$registry_status"
}

probe_kind=
probe_digest=
probe_remote_digest() {
  probe_ref=$1
  probe_kind=error
  probe_digest=
  if registry_call "$registry_timeout" digest "$probe_ref"; then
    probe_digest=$(tr -d '\r\n ' <"$registry_stdout")
    if printf '%s\n' "$probe_digest" | grep -Eq '^sha256:[0-9a-f]{64}$'; then
      probe_kind=found
      return 0
    fi
    last_registry_error="registry returned an invalid digest for $probe_ref"
    return 1
  fi
  if grep -Eqi 'MANIFEST_UNKNOWN|NAME_UNKNOWN|NOT_FOUND|not found|manifest unknown|(^|[^0-9])404([^0-9]|$)' "$registry_stderr"; then
    probe_kind=missing
    return 0
  fi
  return 1
}

probe_with_retry() {
  retry_ref=$1
  retry_number=1
  while test "$retry_number" -le "$max_attempts"; do
    if probe_remote_digest "$retry_ref"; then
      return 0
    fi
    if test "$retry_number" -lt "$max_attempts"; then
      sleep "$retry_delay"
    fi
    retry_number=$((retry_number + 1))
  done
  return 1
}

if ! probe_with_retry "$tag_ref"; then
  fail "cannot resolve immutable runner tag after $max_attempts attempts: $last_registry_error"
fi
case "$probe_kind" in
  found)
    if test "$probe_digest" != "$manifest_digest"; then
      fail "immutable runner tag already resolves to $probe_digest, expected $manifest_digest"
    fi
    reconciled=true
    ;;
  missing)
    publish_number=1
    published=false
    while test "$publish_number" -le "$max_attempts"; do
      publish_attempts=$publish_number
      push_completed=false
      if registry_call "$registry_upload_timeout" push "$work_dir/local-layout" "$tag_ref"; then
        push_completed=true
      fi
      pushed=true
      if ! probe_with_retry "$tag_ref"; then
        fail "runner push outcome could not be reconciled: $last_registry_error"
      fi
      if test "$probe_kind" = found; then
        if test "$probe_digest" != "$manifest_digest"; then
          fail "immutable runner tag resolved to $probe_digest after upload, expected $manifest_digest"
        fi
        if test "$push_completed" != true; then
          reconciled=true
        fi
        published=true
        break
      fi
      if test "$publish_number" -lt "$max_attempts"; then
        sleep "$retry_delay"
      fi
      publish_number=$((publish_number + 1))
    done
    test "$published" = true || fail "runner tag remained missing after $max_attempts bounded upload attempts"
    ;;
  *)
    fail "unexpected registry probe result"
    ;;
esac

if ! probe_with_retry "$digest_ref"; then
  fail "cannot resolve published runner digest: $last_registry_error"
fi
if test "$probe_kind" != found || test "$probe_digest" != "$manifest_digest"; then
  fail "published runner digest reference did not resolve to $manifest_digest"
fi

pull_with_retry() {
  pull_ref=$1
  pull_layout=$2
  pull_number=1
  pull_attempts=0
  while test "$pull_number" -le "$max_attempts"; do
    pull_attempts=$pull_number
    if test -e "$pull_layout"; then
      find "$pull_layout" -depth -delete || fail "cannot reset partial registry read-back layout"
    fi
    if registry_call "$registry_pull_timeout" pull --format=oci "$pull_ref" "$pull_layout"; then
      if test ! -d "$pull_layout" || test -L "$pull_layout"; then
        last_registry_error="crane pull did not create OCI layout directory $pull_layout"
      else
        return 0
      fi
    fi
    test "${registry_status:-0}" -ne 124 || break
    if test "$pull_number" -lt "$max_attempts"; then
      sleep "$retry_delay"
    fi
    pull_number=$((pull_number + 1))
  done
  return 1
}

if ! pull_with_retry "$tag_ref" "$work_dir/tag-readback-layout"; then
  fail "tag read-back failed after $pull_attempts bounded attempt(s): $last_registry_error"
fi
if ! validate_oci_layout "$work_dir/tag-readback-layout" "$manifest_digest" 'tag read-back' "$work_dir/tag-readback.json"; then
  fail "tag read-back graph validation failed"
fi
readback_tag_manifest=$(jq -er '.manifest' "$work_dir/tag-readback.json")
readback_tag_layout_sha=$(jq -er '.layoutSHA256' "$work_dir/tag-readback.json")

if ! pull_with_retry "$digest_ref" "$work_dir/digest-readback-layout"; then
  fail "digest read-back failed after $pull_attempts bounded attempt(s): $last_registry_error"
fi
if ! validate_oci_layout "$work_dir/digest-readback-layout" "$manifest_digest" 'digest read-back' "$work_dir/digest-readback.json"; then
  fail "digest read-back graph validation failed"
fi
readback_digest_manifest=$(jq -er '.manifest' "$work_dir/digest-readback.json")
readback_config_digest=$(jq -er '.configDigest' "$work_dir/digest-readback.json")
readback_layer_count=$(jq -er '.layerCount' "$work_dir/digest-readback.json")
readback_digest_layout_sha=$(jq -er '.layoutSHA256' "$work_dir/digest-readback.json")
test "$readback_tag_manifest" = "$readback_digest_manifest" || fail "tag and digest read-back manifests differ"
test "$readback_config_digest" = "$config_digest" || fail "read-back config digest differs from the local OCI"
test "$readback_layer_count" = "$layer_count" || fail "read-back layer count differs from the local OCI"
if ! probe_with_retry "$tag_ref"; then
  fail "final immutable runner tag check failed: $last_registry_error"
fi
if test "$probe_kind" != found || test "$probe_digest" != "$manifest_digest"; then
  fail "immutable runner tag changed during read-back verification"
fi

completed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
publication_error=
printf 'runner image publication: published %s and verified complete tag/digest read-back\n' "$digest_ref"
write_publication passed 0 || fail "cannot write passing publication state to $publication"
finished=true
