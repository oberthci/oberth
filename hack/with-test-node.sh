#!/bin/sh
# Only test leaves use this runtime; publisher tools are never modified.
set -eu
if [ "$#" -eq 0 ]; then
  echo 'usage: with-test-node.sh command [args...]' >&2
  exit 2
fi
if [ "$(uname -s)/$(uname -m)" != Linux/x86_64 ]; then
  echo 'the test Node pin requires Linux/x86_64' >&2
  exit 1
fi
private=$(mktemp -d /tmp/oberth-test-node.XXXXXXXX)
trap 'rm -rf -- "$private"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir "$private/bin"
archive=$private/node.tar.gz
archive_hash=$(cut -d ' ' -f 1 .oberth/pins/node.sha256)
binary_hash=$(cut -d ' ' -f 1 .oberth/pins/node-bin.sha256)
url=$(cat .oberth/pins/node.url)
verify_archive() {
  printf '%s  %s\n' "$archive_hash" "$archive" | sha256sum -c - >/dev/null 2>&1
}
if [ -n "${OBERTH_TEST_NODE_ARCHIVE:-}" ]; then
  # Release setup already fetched this input. Copy before verification so a
  # concurrent writer cannot change the bytes between hash and extraction.
  cp "$OBERTH_TEST_NODE_ARCHIVE" "$archive"
  verify_archive
  echo 'test Node: verified release input'
else
  # Cache only the archive, never an executable. Every use verifies a private
  # copy. A corrupt/missing entry is repaired from the pinned upstream input.
  cache=${GOCACHE:-$private}/oberth-test-node
  mkdir -p "$cache"
  cached=$cache/$archive_hash.tar.gz
  if cp "$cached" "$archive" 2>/dev/null && verify_archive; then
    echo 'test Node: verified cache hit'
  else
    curl --fail --location --silent --show-error \
      --proto =https --proto-redir =https --tlsv1.2 \
      --connect-timeout 10 --max-time 90 --retry 2 \
      --output "$archive" "$url"
    verify_archive
    # Concurrent fills publish only complete, verified bytes by atomic rename.
    staged=$(mktemp "$cache/.node.XXXXXXXX")
    cp "$archive" "$staged"
    mv -f "$staged" "$cached"
    echo 'test Node: verified download cached'
  fi
fi
prefix=${url##*/}
prefix=${prefix%.tar.gz}
# Extract only the one file to a private regular file; never unpack archive paths.
tar -xOzf "$archive" "$prefix/bin/node" > "$private/bin/node"
printf '%s  %s\n' "$binary_hash" "$private/bin/node" | sha256sum -c -
chmod 700 "$private/bin/node"
PATH="$private/bin:$PATH"
export PATH
node --version
"$@"
