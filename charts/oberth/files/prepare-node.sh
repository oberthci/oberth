#!/bin/sh
set -eu

ci_root=/var/cache/oberth/ci
release_root=/var/cache/oberth/release
owner_uid=65534
owner_gid=65534
check_only=false

fail() {
  printf 'prepare-node: %s\n' "$1" >&2
  exit 1
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --ci-root)
      [ "$#" -ge 2 ] || fail "--ci-root requires a path"
      ci_root=$2
      shift 2
      ;;
    --release-root)
      [ "$#" -ge 2 ] || fail "--release-root requires a path"
      release_root=$2
      shift 2
      ;;
    --uid)
      [ "$#" -ge 2 ] || fail "--uid requires a numeric ID"
      owner_uid=$2
      shift 2
      ;;
    --gid)
      [ "$#" -ge 2 ] || fail "--gid requires a numeric ID"
      owner_gid=$2
      shift 2
      ;;
    --check)
      check_only=true
      shift
      ;;
    *) fail "unknown argument: $1" ;;
  esac
done

case "$owner_uid:$owner_gid" in
  *[!0-9:]*|:*|*:) fail "UID and GID must be numeric" ;;
esac
case "$ci_root:$release_root" in
  *[![:print:]]*) fail "cache roots contain control characters" ;;
esac
case "$ci_root" in /*) ;; *) fail "CI cache root must be absolute" ;; esac
case "$release_root" in /*) ;; *) fail "release cache root must be absolute" ;; esac

command -v realpath >/dev/null 2>&1 || fail "realpath is required"
command -v setpriv >/dev/null 2>&1 || fail "setpriv is required"

# Canonical-form check that also accepts not-yet-created paths on both GNU
# and BusyBox userlands: BusyBox realpath has no -m/-e and no -- terminator,
# and neither implementation resolves paths with missing intermediate
# directories. Symlink traversal in the existing prefix is caught by the
# post-creation realpath equality checks below.
canonical_form() {
  [ "$1" = / ] && return 0
  case "$1" in /*) ;; *) return 1 ;; esac
  case "$1" in */) return 1 ;; esac
  case "$1/" in *//*|*/./*|*/../*) return 1 ;; esac
  return 0
}

canonical_form "$ci_root" || fail "CI cache root must be canonical"
canonical_form "$release_root" || fail "release cache root must be canonical"
[ "$ci_root" != / ] || fail "CI cache root must not be /"
[ "$release_root" != / ] || fail "release cache root must not be /"
[ "$ci_root" != "$release_root" ] || fail "cache roots must be distinct"
case "$release_root" in "$ci_root"/*) fail "release cache root is nested under CI" ;; esac
case "$ci_root" in "$release_root"/*) fail "CI cache root is nested under release" ;; esac

if [ "$check_only" = false ]; then
  if [ "$(id -u)" -ne 0 ] && { [ "$(id -u)" -ne "$owner_uid" ] || [ "$(id -g)" -ne "$owner_gid" ]; }; then
    fail "run as root when assigning another UID or GID"
  fi
  for root in "$ci_root" "$release_root"; do
    directory=$root/repos
    if [ -e "$directory" ] || [ -L "$directory" ]; then
      [ -d "$directory" ] || fail "cache path already exists and is not a directory: $directory"
      [ "$(realpath "$directory")" = "$directory" ] || fail "refusing to follow cache-directory symlink: $directory"
    fi
    install -d -m 0750 -o "$owner_uid" -g "$owner_gid" -- "$directory"
  done
fi

cache_identities=
for root in "$ci_root" "$release_root"; do
  resolved=$(realpath "$root")
  [ "$resolved" = "$root" ] || fail "cache root resolves outside its configured path: $root"
  directory=$root/repos
  [ -d "$directory" ] || fail "missing cache directory: $directory"
  [ "$(realpath "$directory")" = "$directory" ] || fail "cache directory resolves outside its configured path: $directory"
  [ "$(stat -c '%u:%g' -- "$directory")" = "$owner_uid:$owner_gid" ] || fail "wrong owner: $directory"
  [ "$(stat -c '%a' -- "$directory")" = 750 ] || fail "cache directory mode must be 0750: $directory"
  identity=$(stat -Lc '%d:%i' -- "$directory")
  case " $cache_identities " in
    *" $identity "*) fail "cache directories alias the same host inode: $directory" ;;
  esac
  cache_identities="$cache_identities $identity"
  # The probe variables intentionally expand only inside the child shell.
  # shellcheck disable=SC2016
  if [ "$(id -u)" -eq "$owner_uid" ] && [ "$(id -g)" -eq "$owner_gid" ]; then
    sh -c 'probe=$1/.oberth-write-test-$$; (umask 077; : >"$probe") && rm -f -- "$probe"' sh "$directory" \
      || fail "UID $owner_uid cannot write: $directory"
  else
    setpriv --reuid "$owner_uid" --regid "$owner_gid" --clear-groups \
      sh -c 'probe=$1/.oberth-write-test-$$; (umask 077; : >"$probe") && rm -f -- "$probe"' sh "$directory" \
      || fail "UID $owner_uid cannot write: $directory"
  fi
done
