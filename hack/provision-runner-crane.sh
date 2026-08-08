#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
output=$repo_root/bin/crane
tool_tmpdir=${RUNNER_TOOL_TMPDIR:-/var/tmp}
release_url=https://github.com/google/go-containerregistry/releases/download/v0.20.3/go-containerregistry_Linux_x86_64.tar.gz
release_sha=36c67a932f489b3f2724b64af90b599a8ef2aa7b004872597373c0ad694dc059
binary_sha=675f3b2f1696c1f6bc55b1ef535163364119776999f3d1471e4558ed35bab548

fail() {
  printf 'runner crane provision: %s\n' "$*" >&2
  exit 1
}

for command_name in curl sha256sum tar mktemp install find; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done
test -d "$tool_tmpdir" || fail "tool temporary directory does not exist: $tool_tmpdir"
test ! -L "$repo_root/bin" || fail "repository bin directory must not be a symlink"
test ! -L "$output" || fail "runner crane output must not be a symlink"

if test -f "$output" && test -x "$output" && \
   test "$(sha256sum "$output" | cut -d ' ' -f 1)" = "$binary_sha"; then
  exit 0
fi

work_dir=$(mktemp -d "$tool_tmpdir/oberth-runner-crane.XXXXXX") || fail "cannot create tool work directory"
chmod 0700 "$work_dir"
staged_output=
cleanup() {
  cleanup_status=$?
  trap - EXIT HUP INT TERM
  if test -n "$staged_output"; then
    rm -f -- "$staged_output" || cleanup_status=1
  fi
  find "$work_dir" -depth -delete >/dev/null 2>&1 || cleanup_status=1
  exit "$cleanup_status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

curl --fail --silent --show-error --location --max-time 120 \
  --output "$work_dir/crane.tar.gz" "$release_url" || fail "release download failed"
test "$(sha256sum "$work_dir/crane.tar.gz" | cut -d ' ' -f 1)" = "$release_sha" || \
  fail "release archive checksum mismatch"
tar -xzf "$work_dir/crane.tar.gz" -C "$work_dir" crane || fail "release archive extraction failed"
if test ! -f "$work_dir/crane" || test -L "$work_dir/crane" || test ! -x "$work_dir/crane"; then
  fail "release archive did not contain a regular executable crane"
fi
test "$(sha256sum "$work_dir/crane" | cut -d ' ' -f 1)" = "$binary_sha" || \
  fail "crane binary checksum mismatch"
test "$("$work_dir/crane" version 2>/dev/null)" = 0.20.3 || fail "crane version mismatch"

mkdir -p "$repo_root/bin"
staged_output=$repo_root/bin/.crane.$$
install -m 0755 "$work_dir/crane" "$staged_output"
test "$(sha256sum "$staged_output" | cut -d ' ' -f 1)" = "$binary_sha" || \
  fail "installed crane checksum mismatch"
mv -f "$staged_output" "$output"
staged_output=
printf 'runner crane provision: installed verified crane 0.20.3 at %s\n' "$output"
