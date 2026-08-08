#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
output=$repo_root/bin/trivy
tool_tmpdir=${RUNNER_TOOL_TMPDIR:-/var/tmp}
release_url=https://github.com/aquasecurity/trivy/releases/download/v0.73.0/trivy_0.73.0_Linux-64bit.tar.gz
release_sha=2edd39da482bb4e9831962487b68f68e3928ec3137794757f54d00383d79547b
binary_sha=5b3ebab0f98d95196c85efc3a9d31a01520c96fa342e4e611f56db64c516df1d

fail() {
  printf 'runner Trivy provision: %s\n' "$*" >&2
  exit 1
}

for command_name in curl sha256sum tar mktemp install find; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done
test -d "$tool_tmpdir" || fail "tool temporary directory does not exist: $tool_tmpdir"
test ! -L "$repo_root/bin" || fail "repository bin directory must not be a symlink"
test ! -L "$output" || fail "runner Trivy output must not be a symlink"

if test -f "$output" && test -x "$output" && \
   test "$(sha256sum "$output" | cut -d ' ' -f 1)" = "$binary_sha"; then
  exit 0
fi

work_dir=$(mktemp -d "$tool_tmpdir/oberth-runner-trivy.XXXXXX") || fail "cannot create tool work directory"
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

curl --fail --silent --show-error --location --max-time 180 \
  --output "$work_dir/trivy.tar.gz" "$release_url" || fail "release download failed"
test "$(sha256sum "$work_dir/trivy.tar.gz" | cut -d ' ' -f 1)" = "$release_sha" || \
  fail "release archive checksum mismatch"
test "$(tar -tzf "$work_dir/trivy.tar.gz" | grep -cE '^trivy$')" -eq 1 || \
  fail "release archive does not contain exactly one top-level trivy executable"
tar -xzf "$work_dir/trivy.tar.gz" -C "$work_dir" trivy || fail "release archive extraction failed"
if test ! -f "$work_dir/trivy" || test -L "$work_dir/trivy" || test ! -x "$work_dir/trivy"; then
  fail "release archive did not contain a regular executable Trivy"
fi
test "$(sha256sum "$work_dir/trivy" | cut -d ' ' -f 1)" = "$binary_sha" || \
  fail "Trivy binary checksum mismatch"
# Isolate the cache: a populated host trivy DB cache appends "Vulnerability DB"
# lines to --version output, which would break this exact-match sanity check.
# A dedicated empty subdirectory keeps the isolated cache path from colliding
# with the extracted binary at $work_dir/trivy.
test "$(XDG_CACHE_HOME="$work_dir/xdg-cache" "$work_dir/trivy" --version 2>/dev/null)" = 'Version: 0.73.0' || fail "Trivy version mismatch"

mkdir -p "$repo_root/bin"
staged_output=$repo_root/bin/.trivy.$$
install -m 0755 "$work_dir/trivy" "$staged_output"
test "$(sha256sum "$staged_output" | cut -d ' ' -f 1)" = "$binary_sha" || \
  fail "installed Trivy checksum mismatch"
mv -f "$staged_output" "$output"
staged_output=
printf 'runner Trivy provision: installed verified Trivy 0.73.0 at %s\n' "$output"
