#!/bin/sh
set -eu

# CI dependencies belong to this repository. The runner image provides only
# the shell/fetch/unpack substrate and mounts writable Go caches.
umask 077
tools_dir=${OBERTH_TOOLS_DIR:-/tmp/oberth-tools}

fail() {
  echo "install-tools: $*" >&2
  exit 1
}

case "$tools_dir" in
  /tmp/oberth-tools|/tmp/oberth-tools-*) ;;
  *) fail "OBERTH_TOOLS_DIR must be a dedicated /tmp/oberth-tools path" ;;
esac
if [ -L "$tools_dir" ]; then
  fail "OBERTH_TOOLS_DIR must not be a symlink"
fi
mkdir -p "$tools_dir/bin" "$tools_dir/downloads" "$tools_dir/src"
tools_dir=$(readlink -f "$tools_dir")
bin_dir=$tools_dir/bin
downloads_dir=$tools_dir/downloads
sources_dir=$tools_dir/src
download_cache=${OBERTH_DOWNLOAD_CACHE:-}

if [ -n "$download_cache" ]; then
  case "$download_cache" in
    /cache/gobuild) ;;
    /tmp/oberth-download-cache-*) ;;
    *) fail "OBERTH_DOWNLOAD_CACHE must be the mounted repository download cache" ;;
  esac
  [ ! -L "$download_cache" ] || fail "OBERTH_DOWNLOAD_CACHE must not be a symlink"
  if [ ! -d "$download_cache" ] || [ ! -w "$download_cache" ]; then
    fail "OBERTH_DOWNLOAD_CACHE is not writable"
  fi
  if ! resolved_download_cache=$(readlink -f "$download_cache"); then
    fail "OBERTH_DOWNLOAD_CACHE cannot be resolved"
  fi
  [ "$resolved_download_cache" = "$download_cache" ] || fail "OBERTH_DOWNLOAD_CACHE must be canonical"
  download_cache=$resolved_download_cache
fi

sha256_matches() {
  sha_file=$1
  sha_expected=$2
  sha_actual=$(sha256sum "$sha_file" | cut -d ' ' -f 1) || return 1
  [ "$sha_actual" = "$sha_expected" ]
}

verify_sha256() {
  verify_sha_file=$1
  verify_sha_expected=$2
  if ! sha256_matches "$verify_sha_file" "$verify_sha_expected"; then
    verify_sha_actual=$(sha256sum "$verify_sha_file" | cut -d ' ' -f 1) || verify_sha_actual=unreadable
    fail "checksum mismatch for $verify_sha_file: got $verify_sha_actual"
  fi
}

validate_size_pin() {
  size_pin=$1
  case "$size_pin" in
    ''|*[!0-9]*) fail "download size pin must be a positive integer" ;;
  esac
  [ "$size_pin" -gt 0 ] && [ "$size_pin" -le 268435456 ] || fail "download size pin is outside the safety bound"
  printf '%s\n' "$size_pin"
}

expected_download_size() {
  size_pin=$1
  if [ -n "${OBERTH_TEST_DOWNLOAD_SIZE:-}" ]; then
    case "$tools_dir" in
      /tmp/oberth-tools-*) size_pin=$OBERTH_TEST_DOWNLOAD_SIZE ;;
      *) fail "OBERTH_TEST_DOWNLOAD_SIZE is allowed only for an isolated test tools directory" ;;
    esac
  fi
  validate_size_pin "$size_pin"
}

verify_size() {
  size_file=$1
  size_expected=$2
  size_actual=$(busybox stat -c %s "$size_file") || fail "cannot inspect size of $size_file"
  [ "$size_actual" -eq "$size_expected" ] || fail "size mismatch for $size_file: got $size_actual"
}

restore_cached_download() {
  output=$1
  expected=$2
  name=$3
  expected_size=$4
  slot=$5
  [ -n "$download_cache" ] || return 1
  cached=$download_cache/.oberth-download-$slot
  [ -f "$cached" ] && [ ! -L "$cached" ] || return 1
  copy_limit=$((expected_size + 1))
  # Hold the selected inode open while copying. The separate timeout bounds
  # the open itself, so a sibling cannot replace the checked path with a FIFO
  # and block the installer. Identity checks reject path replacement while the
  # descriptor keeps later replacement from redirecting the bounded read.
  # shellcheck disable=SC2016 # Variables belong to the bounded child shell.
  if ! busybox timeout 10 busybox sh -c '
    cached_path=$1
    partial_path=$2
    pinned_size=$3
    bounded_size=$4
    exec 3<"$cached_path" || exit 1
    [ ! -L "$cached_path" ] && [ -f "$cached_path" ] || exit 1
    path_identity=$(busybox stat -Lc "%d:%i" "$cached_path") || exit 1
    descriptor_identity=$(busybox stat -Lc "%d:%i" /proc/self/fd/3) || exit 1
    [ "$path_identity" = "$descriptor_identity" ] || exit 1
    descriptor_size=$(busybox stat -Lc %s /proc/self/fd/3) || exit 1
    [ "$descriptor_size" -eq "$pinned_size" ] || exit 1
    busybox head -c "$bounded_size" <&3 >"$partial_path" || exit 1
    exec 3<&-
  ' oberth-cache-copy "$cached" "$partial" "$expected_size" "$copy_limit"; then
    rm -f -- "$partial"
    return 1
  fi
  if ! local_size=$(busybox stat -c %s "$partial"); then
    rm -f -- "$partial"
    return 1
  fi
  if [ "$local_size" -ne "$expected_size" ]; then
    rm -f -- "$partial"
    return 1
  fi
  if ! sha256_matches "$partial" "$expected"; then
    rm -f -- "$partial"
    return 1
  fi
  if ! mv "$partial" "$output"; then
    rm -f -- "$partial"
    return 1
  fi
  printf 'install-tools: restored verified download %s\n' "$name" >&2
}

# Only the creator releases this lock. Shell pathname checks cannot perform a
# safe stale-owner CAS across Pods, so a crash-left lock remains as one bounded
# cache miss instead of being deleted by another untrusted Job.
acquire_cache_publication_lock() {
  cache_lock=$download_cache/.oberth-download-$1.lock
  if ! mkdir "$cache_lock" 2>/dev/null; then
    cache_lock=
    cache_lock_identity=
    return 1
  fi
  if ! cache_lock_identity=$(busybox stat -Lc '%d:%i' "$cache_lock"); then
    rmdir "$cache_lock" 2>/dev/null || true
    cache_lock=
    cache_lock_identity=
    return 1
  fi
}

release_cache_publication_lock() {
  lock_release_status=0
  if [ -z "$cache_lock" ] || [ -z "$cache_lock_identity" ] ||
    [ -L "$cache_lock" ] || [ ! -d "$cache_lock" ] ||
    ! current_lock_identity=$(busybox stat -Lc '%d:%i' "$cache_lock") ||
    [ "$current_lock_identity" != "$cache_lock_identity" ]; then
    cache_partial=
    cache_lock=
    cache_lock_identity=
    return 1
  fi
  if [ -n "$cache_partial" ] && ! rm -f -- "$cache_partial"; then
    lock_release_status=1
  fi
  if [ -n "$cache_lock" ] && ! rmdir "$cache_lock" 2>/dev/null; then
    lock_release_status=1
  fi
  cache_partial=
  cache_lock=
  cache_lock_identity=
  [ "$lock_release_status" -eq 0 ]
}

publish_cached_download() {
  output=$1
  expected=$2
  expected_size=$3
  slot=$4
  [ -n "$download_cache" ] || return 0
  if ! acquire_cache_publication_lock "$slot"; then
    printf 'install-tools: download cache publication unavailable for %s\n' "$slot" >&2
    return 0
  fi
  # A fixed stage bounds crash residue to one artifact per slot. A hard crash
  # leaves a cache miss behind its fixed lock; no commit, PID, or retry creates
  # another persistent path.
  cache_partial=$download_cache/.oberth-download-$slot.stage
  # As with restore, keep all writes on a verified descriptor inside a bound.
  # Opening read/write does not truncate a symlink target before the no-symlink
  # and inode-identity checks; only the validated descriptor is then reset.
  # shellcheck disable=SC2016 # Variables belong to the bounded child shell.
  if ! busybox timeout 10 busybox sh -c '
    source_path=$1
    staged_path=$2
    pinned_size=$3
    pinned_hash=$4
    exec 3<>"$staged_path" || exit 1
    [ ! -L "$staged_path" ] && [ -f "$staged_path" ] || exit 1
    path_identity=$(busybox stat -Lc "%d:%i" "$staged_path") || exit 1
    descriptor_identity=$(busybox stat -Lc "%d:%i" /proc/self/fd/3) || exit 1
    [ "$path_identity" = "$descriptor_identity" ] || exit 1
    : >/proc/self/fd/3 || exit 1
    busybox cat "$source_path" >&3 || exit 1
    descriptor_size=$(busybox stat -Lc %s /proc/self/fd/3) || exit 1
    [ "$descriptor_size" -eq "$pinned_size" ] || exit 1
    actual_hash=$(sha256sum /proc/self/fd/3 | cut -d " " -f 1) || exit 1
    [ "$actual_hash" = "$pinned_hash" ] || exit 1
    final_identity=$(busybox stat -Lc "%d:%i" "$staged_path") || exit 1
    [ "$final_identity" = "$descriptor_identity" ] || exit 1
    exec 3<&-
  ' oberth-cache-stage "$output" "$cache_partial" "$expected_size" "$expected" ||
    ! mv -fT "$cache_partial" "$download_cache/.oberth-download-$slot"; then
    if ! release_cache_publication_lock; then
      printf 'install-tools: bounded download cache stage remains for %s\n' "$slot" >&2
    fi
    printf 'install-tools: download cache publication unavailable for %s\n' "$slot" >&2
    return 0
  fi
  cache_partial=
  if ! release_cache_publication_lock; then
    printf 'install-tools: bounded download cache lock remains for %s\n' "$slot" >&2
  fi
}

cleanup_fetch() {
  rm -f -- "$partial" "$curl_status_file"
  if [ -n "$cache_partial" ] || [ -n "$cache_lock" ]; then
    release_cache_publication_lock || true
  fi
}

fetch_verified() {
  url=$1
  expected=$2
  name=$3
  if ! expected_size=$(expected_download_size "$4"); then
    return 1
  fi
  slot=$5
  case "$slot" in
    go|python|zig|golangci|trivy|helm|cosign) ;;
    *) fail "download cache slot is invalid" ;;
  esac
  output=$downloads_dir/$name
  if [ -f "$output" ]; then
    verify_size "$output" "$expected_size"
    verify_sha256 "$output" "$expected"
  else
    partial=$output.partial.$$
    curl_status_file=$partial.status
    cache_partial=
    cache_lock=
    cache_lock_identity=
    rm -f -- "$partial" "$curl_status_file"
    trap cleanup_fetch EXIT HUP INT TERM
    if ! restore_cached_download "$output" "$expected" "$name" "$expected_size" "$slot"; then
      if (
        if curl --fail --location --progress-bar --show-error \
          --connect-timeout 10 --max-time 90 \
          --retry 2 --retry-all-errors --retry-max-time 180 \
          --proto '=https' --proto-redir '=https' --tlsv1.2 \
          --output "$partial" "$url"; then
          curl_status=0
        else
          curl_status=$?
        fi
        printf '%s\n' "$curl_status" >"$curl_status_file"
      ) 2>&1 | busybox awk 'BEGIN { RS = "\r" } { print; fflush() }' >&2; then
        progress_status=0
      else
        progress_status=$?
      fi
      [ "$progress_status" -eq 0 ] || fail "curl progress filter failed with status $progress_status"
      [ -f "$curl_status_file" ] || fail "curl status record is absent"
      IFS= read -r curl_status <"$curl_status_file" || fail "curl status record is unreadable"
      rm -f -- "$curl_status_file"
      case "$curl_status" in
        ''|*[!0-9]*) fail "curl status record is invalid" ;;
      esac
      [ "$curl_status" -eq 0 ] || return "$curl_status"
      verify_size "$partial" "$expected_size"
      verify_sha256 "$partial" "$expected"
      if ! mv "$partial" "$output"; then
        fail "cannot install verified download $name"
      fi
      publish_cached_download "$output" "$expected" "$expected_size" "$slot"
    fi
    trap - EXIT HUP INT TERM
  fi
  printf '%s\n' "$output"
}

install_gzip_binary() {
  archive=$1
  binary_name=$2
  binary_size=$(validate_size_pin "$3")
  binary_sha=$4
  target=$bin_dir/$binary_name
  [ ! -e "$target" ] || fail "existing $binary_name installation is unexpected"
  staged=$bin_dir/.$binary_name.$$
  [ ! -e "$staged" ] || fail "$binary_name staging path already exists"
  cleanup_gzip_binary() {
    rm -f -- "$staged"
  }
  trap cleanup_gzip_binary EXIT HUP INT TERM
  if ! busybox gzip -dc "$archive" >"$staged"; then
    fail "$binary_name archive extraction failed"
  fi
  verify_size "$staged" "$binary_size"
  verify_sha256 "$staged" "$binary_sha"
  chmod 0755 "$staged"
  mv "$staged" "$target"
  trap - EXIT HUP INT TERM
}

install_go() {
  version=1.26.5
  archive=$(fetch_verified \
    "https://go.dev/dl/go1.26.5.linux-amd64.tar.gz" \
    "5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053" \
    "go${version}.linux-amd64.tar.gz" \
    "66879095" \
    "go")
  if [ ! -x "$tools_dir/go/bin/go" ]; then
    [ ! -e "$tools_dir/go" ] || fail "existing Go installation is incomplete"
    stage=$tools_dir/.go.$$
    mkdir "$stage"
    tar -xzf "$archive" -C "$stage"
    [ -x "$stage/go/bin/go" ] || fail "Go archive lacks go/bin/go"
    mv "$stage/go" "$tools_dir/go"
    rmdir "$stage"
  fi
  ln -sf ../go/bin/go "$bin_dir/go"
  ln -sf ../go/bin/gofmt "$bin_dir/gofmt"
  "$bin_dir/go" version | grep -Fx "go version go${version} linux/amd64" >/dev/null || \
    fail "installed Go version is not go${version} linux/amd64"
}

install_python() {
  version=3.13.14
  build=20260718
  archive=$(fetch_verified \
    "https://github.com/astral-sh/python-build-standalone/releases/download/20260718/cpython-3.13.14%2B20260718-x86_64-unknown-linux-musl-install_only_stripped.tar.gz" \
    "0dbfe4b9c043f81a60953245a2735594f6b09308031086ceac15511f4c0af193" \
    "cpython-${version}+${build}-x86_64-unknown-linux-musl-install_only_stripped.tar.gz" \
    "27710872" \
    "python")
  [ ! -L "$tools_dir/python" ] || fail "existing Python installation must not be a symlink"
  if [ ! -x "$tools_dir/python/bin/python3" ]; then
    [ ! -e "$tools_dir/python" ] || fail "existing Python installation is incomplete"
    stage=$tools_dir/.python.$$
    mkdir "$stage"
    trap 'test ! -d "$stage" || rm -r -- "$stage"' EXIT HUP INT TERM
    tar -xzf "$archive" -C "$stage"
    verify_python_installation "$stage/python"
    unexpected=$(find "$stage" -mindepth 1 -maxdepth 1 ! -name python -print -quit)
    [ -z "$unexpected" ] || fail "Python archive has an unexpected top-level entry"
    mv "$stage/python" "$tools_dir/python"
    rmdir "$stage"
    trap - EXIT HUP INT TERM
  fi
  verify_python_installation "$tools_dir/python"
  ln -sf ../python/bin/python3 "$bin_dir/python3"
}

verify_python_installation() {
  python_root=$1
  [ -d "$python_root" ] && [ ! -L "$python_root" ] || \
    fail "Python installation root must be a real directory"
  [ -d "$python_root/bin" ] && [ ! -L "$python_root/bin" ] || \
    fail "Python bin path must be a real directory"
  [ -f "$python_root/bin/python3.13" ] && [ ! -L "$python_root/bin/python3.13" ] && \
    [ -x "$python_root/bin/python3.13" ] || fail "Python installation lacks a regular python3.13 executable"
  [ -L "$python_root/bin/python3" ] && \
    [ "$(readlink "$python_root/bin/python3")" = "python3.13" ] || \
    fail "Python installation has an unexpected python3 entry point"
  verify_sha256 "$python_root/bin/python3.13" \
    "c524653b6cc492851f59bf5572c81f3f9b518fa84b34e734c59ba06848d65a8f"
  "$python_root/bin/python3.13" -I -S -B -c \
    'import platform, sys; raise SystemExit(platform.machine() != "x86_64" or sys.version_info[:3] != (3, 13, 14))' || \
    fail "installed Python version is not 3.13.14 linux/amd64"
}

install_zig() {
  install_xz
  version=0.14.1
  archive=$(fetch_verified \
    "https://ziglang.org/download/0.14.1/zig-x86_64-linux-0.14.1.tar.xz" \
    "24aeeec8af16c381934a6cd7d95c807a8cb2cf7df9fa40d359aa884195c4716c" \
    "zig-x86_64-linux-${version}.tar.xz" \
    "49086504" \
    "zig")
  if [ ! -x "$tools_dir/zig/zig" ]; then
    [ ! -e "$tools_dir/zig" ] || fail "existing Zig installation is incomplete"
    stage=$tools_dir/.zig.$$
    mkdir "$stage"
    decompressed=$stage/zig.tar
    "$bin_dir/xz" -d -c "$archive" >"$decompressed"
    tar -xf "$decompressed" -C "$stage"
    rm "$decompressed"
    extracted=$stage/zig-x86_64-linux-$version
    [ -x "$extracted/zig" ] || fail "Zig archive lacks the zig executable"
    mv "$extracted" "$tools_dir/zig"
    rmdir "$stage"
  fi
  ln -sf ../zig/zig "$bin_dir/zig"
  cat >"$bin_dir/gcc" <<'EOF'
#!/bin/sh
tools_dir=${OBERTH_TOOLS_DIR:-/tmp/oberth-tools}
exec "$tools_dir/zig/zig" cc -target x86_64-linux-musl "$@"
EOF
  cat >"$bin_dir/g++" <<'EOF'
#!/bin/sh
tools_dir=${OBERTH_TOOLS_DIR:-/tmp/oberth-tools}
exec "$tools_dir/zig/zig" c++ -target x86_64-linux-musl "$@"
EOF
  chmod 0755 "$bin_dir/gcc" "$bin_dir/g++"
  [ "$("$bin_dir/zig" version)" = "$version" ] || fail "installed Zig version is not $version"
}

require_go() {
  [ -x "$tools_dir/go/bin/go" ] || fail "install Go before source-built tools"
  [ ! -L "$tools_dir/gopath" ] || fail "repository-owned GOPATH must not be a symlink"
  mkdir -p "$tools_dir/gopath"
  [ -d "$tools_dir/gopath" ] && [ -w "$tools_dir/gopath" ] || \
    fail "repository-owned GOPATH is not writable"
  go_cmd=$tools_dir/go/bin/go
  GOPATH=$tools_dir/gopath
  export GOPATH GOTOOLCHAIN=local GOWORK=off
}

download_module_metadata() {
  module_to_download=$1
  download_attempt=1
  while ! metadata=$("$go_cmd" mod download -json "$module_to_download"); do
    if [ "$download_attempt" -ge 3 ]; then
      [ -z "$metadata" ] || printf '%s\n' "$metadata" >&2
      fail "$module_to_download download failed after $download_attempt attempts"
    fi
    sleep "$download_attempt"
    download_attempt=$((download_attempt + 1))
  done
}

prepare_module_source() {
  name=$1
  module=$2
  module_sum=$3
  module_mod_sum=$4
  require_go
  download_module_metadata "$module"
  printf '%s\n' "$metadata" | grep -F '"Sum": "'"$module_sum"'"' >/dev/null || \
    fail "$module source checksum does not match $module_sum"
  printf '%s\n' "$metadata" | grep -F '"GoModSum": "'"$module_mod_sum"'"' >/dev/null || \
    fail "$module go.mod checksum does not match $module_mod_sum"
  module_path=${module%@*}
  module_version=${module##*@}
  module_dir=$("$go_cmd" env GOMODCACHE)/$module_path@$module_version
  [ -d "$module_dir" ] || fail "downloaded module directory is absent: $module_dir"
  tool_source=$sources_dir/$name.$$
  [ ! -e "$tool_source" ] || fail "tool source path already exists: $tool_source"
  mkdir "$tool_source"
  cp -R "$module_dir"/. "$tool_source"/
  chmod -R u+w "$tool_source"
}

discard_module_source() {
  case "$tool_source" in
    "$sources_dir"/*) rm -r -- "$tool_source" ;;
    *) fail "refusing to remove unexpected tool source path" ;;
  esac
}

install_xz() {
  xz_version=0.5.16
  xz_checksum=ac4688de237077cbade4c16cfc2ab32f22ba2ff4ffb8bc1ad39ade32dd09e696
  if [ -e "$bin_dir/xz" ]; then
    [ ! -L "$bin_dir/xz" ] && [ -f "$bin_dir/xz" ] && [ -x "$bin_dir/xz" ] || \
      fail "existing xz installation is not a regular executable"
    verify_sha256 "$bin_dir/xz" "$xz_checksum"
    "$bin_dir/xz" --version 2>&1 | grep -F "version v${xz_version}" >/dev/null || \
      fail "installed pure-Go xz version is not v${xz_version}"
    return
  fi
  prepare_module_source xz \
    "github.com/ulikunitz/xz@v0.5.16" \
    "h1:ld6NyySjx5lowVKwJvMRLnW5nxKX/xnpSiFYZ/Lxur0=" \
    "h1:H9Rt/W6/Qj27PGauhQc6nfCDy7vHpzsOThBSaYDoEhw="
  staged_xz=$bin_dir/.xz.$$
  [ ! -e "$staged_xz" ] || fail "xz staging path already exists"
  (
    cd "$tool_source"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$go_cmd" build \
      -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
      -o "$staged_xz" ./cmd/gxz
  )
  verify_sha256 "$staged_xz" "$xz_checksum"
  "$staged_xz" --version 2>&1 | grep -F "version v${xz_version}" >/dev/null || \
    fail "installed pure-Go xz version is not v${xz_version}"
  mv "$staged_xz" "$bin_dir/xz"
  discard_module_source
}

install_golangci() {
  require_go
  archive=$(fetch_verified \
    "https://releases.cloudtaser.io/oberth/toolchains/sha256-5675ce9cba0d39c751a6c4302b26d79a38cc4904e8eba586395b70796acb16a0/golangci-lint-2.12.2-oberth.1-linux-amd64.gz" \
    "5675ce9cba0d39c751a6c4302b26d79a38cc4904e8eba586395b70796acb16a0" \
    "golangci-lint-2.12.2-oberth.1-linux-amd64.gz" \
    "14783489" \
    "golangci")
  install_gzip_binary "$archive" golangci-lint "40685694" \
    "30d5e2e3715aed6e3a1008c9e074e50291926213d1b4fc50fa27e08ccaed7438"
  "$go_cmd" version -m "$bin_dir/golangci-lint" | grep -E 'dep[[:space:]]+golang.org/x/text[[:space:]]+v0.39.0' >/dev/null || \
    fail "golangci-lint lacks the patched x/text dependency"
  "$bin_dir/golangci-lint" version | grep -F 'version 2.12.2+oberth.1' >/dev/null || \
    fail "golangci-lint derivative identity is wrong"
}

install_trivy() {
  require_go
  archive=$(fetch_verified \
    "https://releases.cloudtaser.io/oberth/toolchains/sha256-9faca4cd2dc9a1acd72e195ff1f5fed084b5cb0b11086aa3b2ffca7420e6c029/trivy-0.73.0-oberth.1-linux-amd64.gz" \
    "9faca4cd2dc9a1acd72e195ff1f5fed084b5cb0b11086aa3b2ffca7420e6c029" \
    "trivy-0.73.0-oberth.1-linux-amd64.gz" \
    "48757831" \
    "trivy")
  install_gzip_binary "$archive" trivy "166244478" \
    "cce775a873e9df2468a5578148907380cf1f07b4c27ecec25d9b1da10dec0406"
  "$go_cmd" version -m "$bin_dir/trivy" | grep -E 'dep[[:space:]]+oras.land/oras-go/v2[[:space:]]+v2.6.2' >/dev/null || \
    fail "Trivy lacks the patched oras dependency"
  "$bin_dir/trivy" --version | grep -Fx 'Version: 0.73.0+oberth.1' >/dev/null || \
    fail "Trivy derivative identity is wrong"
}

install_helm() {
  require_go
  archive=$(fetch_verified \
    "https://releases.cloudtaser.io/oberth/toolchains/sha256-e04e9b95e7a8377354b74f727033731ee095e8316401c8ac2423b058e614b1bd/helm-4.2.3-oberth.1-linux-amd64.gz" \
    "e04e9b95e7a8377354b74f727033731ee095e8316401c8ac2423b058e614b1bd" \
    "helm-4.2.3-oberth.1-linux-amd64.gz" \
    "19653291" \
    "helm")
  install_gzip_binary "$archive" helm "64270462" \
    "ae8c0c2bcd766239f8ef834e622449a1d1261a33e63e63287dfb4f9dc4072cd9"
  "$go_cmd" version -m "$bin_dir/helm" | grep -E 'dep[[:space:]]+oras.land/oras-go/v2[[:space:]]+v2.6.2' >/dev/null || \
    fail "Helm lacks the patched oras dependency"
  "$bin_dir/helm" version --short | grep -Fx 'v4.2.3+oberth.1.g43e8b7f' >/dev/null || \
    fail "Helm derivative identity is wrong"
}

install_cosign() {
	version=3.1.1
	binary=$(fetch_verified \
		"https://github.com/sigstore/cosign/releases/download/v${version}/cosign-linux-amd64" \
		"ae1ecd212663f3693ad9edf8b1a183900c9a52d3155ba6e354237f9a0f6463fc" \
		"cosign-linux-amd64-${version}" \
		"140588378" \
		"cosign")
	cp "$binary" "$bin_dir/cosign"
	chmod 0755 "$bin_dir/cosign"
	verify_sha256 "$bin_dir/cosign" "ae1ecd212663f3693ad9edf8b1a183900c9a52d3155ba6e354237f9a0f6463fc"
	"$bin_dir/cosign" version 2>&1 | grep -F "GitVersion:    v${version}" >/dev/null || \
		fail "installed cosign version is not v${version}"
}

install_one() {
  case "$1" in
    go) install_go ;;
    python) install_python ;;
    xz) install_xz ;;
    zig) install_zig ;;
    golangci) install_golangci ;;
		trivy) install_trivy ;;
		helm) install_helm ;;
		cosign) install_cosign ;;
    *) fail "unknown tool $1" ;;
  esac
}

[ "$#" -gt 0 ] || fail "usage: $0 go|python|xz|zig|golangci|trivy|helm|cosign [tool ...]"
for tool in "$@"; do
  install_one "$tool"
done
