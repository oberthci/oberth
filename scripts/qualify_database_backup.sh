#!/bin/sh
# Mandatory tagged integration entrypoint. Run only inside the documented,
# retained no-swap sandbox; the test independently refuses missing protection
# before generating any synthetic recipient. This script is not a host launcher.
set -eu
export PATH=/usr/bin:/bin
test "$#" -eq 2 || exit 2
binary=$1
expected=$2
case "$binary" in /*) ;; *) exit 2 ;; esac
case "$binary" in *[!a-zA-Z0-9_./-]*) exit 2 ;; esac
case "$expected" in *[!0-9a-f]*) exit 2 ;; esac
test "${#expected}" -eq 64 || exit 2
test -f "$binary" && test -x "$binary" && ! test -L "$binary" || exit 2
actual=$(sha256sum -- "$binary")
test "${actual%% *}" = "$expected" || exit 2
exec env -i PATH=/usr/bin:/bin HOME=/tmp TMPDIR=/tmp \
  OBERTH_BACKUP_SYNTHETIC=1 GOMAXPROCS=1 \
  "$binary" '-test.run=^TestEncryptedBackupActualCMS$' \
  -test.count=1 -test.v -test.timeout=90s
