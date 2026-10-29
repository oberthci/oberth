#!/bin/sh
set -eu
pins=$1
patch_file=$2
output=$3
# shellcheck disable=SC1090
. "$pins"
build_dir=${4:-/git}
mkdir -p "$build_dir" "$output"
test -z "$(ls -A "$build_dir")"
wget -qO /tmp/oberth-git-source.tar.xz "https://www.kernel.org/pub/software/scm/git/git-$GIT_VERSION.tar.xz"
printf '%s  %s\n' "$GIT_SOURCE_SHA256" /tmp/oberth-git-source.tar.xz | sha256sum -c -
printf '%s  %s\n' "$GIT_PATCH_SHA256" "$patch_file" | sha256sum -c -
# Root CI containers lack CAP_CHOWN; do not restore archive ownership.
tar --no-same-owner -xJf /tmp/oberth-git-source.tar.xz -C "$build_dir" --strip-components=1
rm /tmp/oberth-git-source.tar.xz
cd "$build_dir"
patch -p1 --fuzz=0 < "$patch_file"
make -j2 git "CC=${CC:-cc}" prefix=/usr CFLAGS="-O2 -ffile-prefix-map=$build_dir=." LDFLAGS=-static \
  NO_CURL=YesPlease NO_OPENSSL=YesPlease NO_EXPAT=YesPlease \
  NO_PERL=YesPlease NO_PYTHON=YesPlease NO_TCLTK=YesPlease NO_GETTEXT=YesPlease \
  NO_ICONV=YesPlease NO_LIBPCRE2=YesPlease NO_REGEX=YesPlease NO_RUST=YesPlease
cp git "$output/git"
strip "$output/git"
program_headers=$(readelf -l "$output/git")
case "$program_headers" in *INTERP*) echo 'static Git has an interpreter' >&2; exit 1;; esac
dynamic_headers=$(readelf -d "$output/git")
case "$dynamic_headers" in *NEEDED*) echo 'static Git has dynamic dependencies' >&2; exit 1;; esac
