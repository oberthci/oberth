#!/bin/sh
# Hermetic package-manager dispatch test. Relocate only the absolute binary
# path into a private fixture; execute the installer with the exact server PATH.
set -eu
source_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
script=${1:-$source_root/hack/install-static-git-packages.sh}
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
apk_fixture=$scratch/apk
export apk_test_log="$scratch/args"
server_path=/tmp/oberth-tools/bin:/usr/local/bin:/usr/bin:/bin
cat > "$scratch/pins" <<'PINS'
APK_PACKAGES='apk-tools=3.0.8-r0 musl=1.2.5-r24 zlib=1.3.2-r1'
PINS
cat > "$apk_fixture" <<'APK'
#!/bin/sh
case "$1" in
  --version) printf '%s\n' 'apk-tools 3.0.8-r0, compiled for x86_64.' ;;
  *) printf '%s\n' "$@" > "$apk_test_log" ;;
esac
APK
chmod 755 "$apk_fixture"
sed "s|/sbin/apk|$apk_fixture|g" "$script" > "$scratch/install.sh"
# Both the writable image build and the read-only-root CI install must dispatch
# to the absolute fixture, despite /sbin being absent from this exact PATH.
PATH=$server_path /bin/sh "$scratch/install.sh" "$scratch/pins"
printf '%s\n' add --no-cache apk-tools=3.0.8-r0 musl=1.2.5-r24 zlib=1.3.2-r1 > "$scratch/want"
cmp "$scratch/want" "$apk_test_log"
PATH=$server_path /bin/sh "$scratch/install.sh" "$scratch/pins" "$scratch/root"
printf '%s\n' --root "$scratch/root" --initdb --no-scripts --keys-dir /etc/apk/keys \
  --repositories-file /etc/apk/repositories add --no-cache apk-tools=3.0.8-r0 musl=1.2.5-r24 zlib=1.3.2-r1 > "$scratch/want"
cmp "$scratch/want" "$apk_test_log"
rm "$apk_test_log"
# A same-name executable with a different version is not the pinned apk-tools.
sed 's/3.0.8-r0/3.0.7-r0/' "$apk_fixture" > "$scratch/wrong-apk"
mv "$scratch/wrong-apk" "$apk_fixture"
chmod 755 "$apk_fixture"
if PATH=$server_path /bin/sh "$scratch/install.sh" "$scratch/pins" > "$scratch/out" 2>&1; then
  echo 'unexpected apk binary was accepted' >&2; exit 1
fi
grep -q 'does not match apk-tools=3.0.8-r0' "$scratch/out"
[ ! -f "$apk_test_log" ]
chmod 644 "$apk_fixture"
if PATH=$server_path /bin/sh "$scratch/install.sh" "$scratch/pins" > "$scratch/out" 2>&1; then
  echo 'non-executable apk was accepted' >&2; exit 1
fi
grep -q 'expected a regular executable' "$scratch/out"
rm "$apk_fixture"
if PATH=$server_path /bin/sh "$scratch/install.sh" "$scratch/pins" > "$scratch/out" 2>&1; then
  echo 'missing apk was accepted' >&2; exit 1
fi
grep -q 'expected a regular executable' "$scratch/out"
printf '%s\n' 'static Git package dispatch: exact server PATH, both install modes and binary refusal checks passed'
