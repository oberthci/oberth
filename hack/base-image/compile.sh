#!/bin/sh
# Compile only the native Git executable; its transports use the product shim.
set -eu
# shellcheck disable=SC1091
. /inputs/build.env
printf '%s  %s\n' "$GIT_SOURCE_SHA256" /inputs/git.tar.xz | sha256sum -c -
printf '%s  %s\n' "$GIT_PATCH_SHA256" /inputs/git-no-shell.patch | sha256sum -c -
test "$(cat /etc/alpine-release)" = "$ALPINE_VERSION"
tar --no-same-owner -xJf /inputs/git.tar.xz -C /git --strip-components=1
patch --directory=/git -p1 --fuzz=0 < /inputs/git-no-shell.patch
make -C /git -j2 git CC=cc prefix=/usr CFLAGS='-O2 -ffile-prefix-map=/git=.' LDFLAGS=-static \
    NO_CURL=YesPlease NO_OPENSSL=YesPlease NO_EXPAT=YesPlease \
    NO_PERL=YesPlease NO_PYTHON=YesPlease NO_TCLTK=YesPlease NO_GETTEXT=YesPlease \
    NO_ICONV=YesPlease NO_LIBPCRE2=YesPlease NO_REGEX=YesPlease NO_RUST=YesPlease
mkdir /out
cp /git/git /out/git
strip /out/git
cp /usr/lib/libc.a /out/musl.a
cp /usr/lib/libz.a /out/zlib.a
cp /etc/ssl/certs/ca-certificates.crt /out/ca-certificates.crt
cp /lib/apk/db/installed /out/installed-packages.txt
