#!/bin/sh
set -eu

script=${1:-hack/prepare-node.sh}
test_root=$(mktemp -d)
trap 'rm -rf -- "$test_root"' EXIT

ci_root=$test_root/ci
release_root=$test_root/release
owner_uid=$(id -u)
owner_gid=$(id -g)

"$script" \
  --ci-root "$ci_root" \
  --release-root "$release_root" \
  --uid "$owner_uid" \
  --gid "$owner_gid"
"$script" \
  --ci-root "$ci_root" \
  --release-root "$release_root" \
  --uid "$owner_uid" \
  --gid "$owner_gid" \
  --check

for root in "$ci_root" "$release_root"; do
  test -d "$root/repos"
  test "$(stat -c '%u:%g' -- "$root/repos")" = "$owner_uid:$owner_gid"
  test "$(stat -c '%a' -- "$root/repos")" = 750
  test ! -e "$root/gomod"
  test ! -e "$root/gobuild"
  test ! -e "$root/golangci-lint"
  test "$(find "$root" -mindepth 1 -maxdepth 1 -type d | wc -l)" -eq 1
done

rmdir "$ci_root/repos"
ln -s "$release_root/repos" "$ci_root/repos"
if "$script" \
  --ci-root "$ci_root" \
  --release-root "$release_root" \
  --uid "$owner_uid" \
  --gid "$owner_gid" >/dev/null 2>&1; then
  exit 1
fi
test "$(stat -c '%a' -- "$release_root/repos")" = 750
rm -- "$ci_root/repos"
install -d -m 0750 -o "$owner_uid" -g "$owner_gid" -- "$ci_root/repos"

if "$script" \
  --ci-root "$ci_root" \
  --release-root "$ci_root/release" \
  --uid "$owner_uid" \
  --gid "$owner_gid" >/dev/null 2>&1; then
  exit 1
fi
