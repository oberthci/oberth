#!/bin/sh
set -eu

# All build, scan, signing, registry, and chart behavior for an Oberth
# release lives here; the Job substrate is the repository-declared golang
# image and every tool below arrives through the pinned setup steps.
umask 077

action=${1:-}
tag=${OBERTH_RELEASE_TAG:-}
sha=${OBERTH_RELEASE_SHA:-}
release_dir=${OBERTH_RELEASE_DIR:-/tmp/oberth-release}
# Release credentials are store-sourced (SecretStoreSecrets in periapsis.go)
# and delivered under $OBERTH_SECRETSTORE_DIR/<name>/<key> in the release Pod;
# a mounted Kubernetes-Secret root at /secrets remains the final fallback.
secret_root=${OBERTH_SECRET_ROOT:-${OBERTH_SECRETSTORE_DIR:-/secrets}}
# Derived credential files (R2 curl config, Docker/Helm registry auth,
# Homebrew tap SSH key) are written to a scratch directory under the same
# memory-backed mount.  This prevents credential material from landing on
# the work PVC (node disk), where a SIGKILL would skip the exit-trap cleanup.
cred_dir=${OBERTH_CREDENTIAL_DIR:-$secret_root/.runtime}
release_origin=${OBERTH_RELEASE_ORIGIN:-https://releases.oberth.ci}
chart_repo=${OBERTH_CHART_REPOSITORY:-https://charts.oberth.ci}
r2_account=${OBERTH_R2_ACCOUNT:-0bc3ad9e8a1ef560fa5f1536d0696bc7}
r2_bucket=${OBERTH_R2_BUCKET:-oberth-releases}
r2_endpoint=${OBERTH_R2_ENDPOINT:-https://${r2_account}.r2.cloudflarestorage.com}
r2_curl_config=${OBERTH_R2_CURL_CONFIG:-$cred_dir/.r2-curl.conf}
gar_host=europe-west4-docker.pkg.dev
gar_chart_oci=oci://${gar_host}/skipopsmain/oberth-helm
gar_chart=${gar_host}/skipopsmain/oberth-helm/oberth
# Secret file names below are the store's OWN KV field names, verbatim:
# `oberth secretstore exec` writes $OBERTH_SECRETSTORE_DIR/<path-base>/<field>
# with no renaming layer. The fields carry env-var-style names (GAR_SA_KEY,
# R2_UPLOAD_TOKEN, COSIGN_KEY, COSIGN_PUB, SSH_KEY) because the retired
# envconsul chain injected each field verbatim as an environment variable
# (docs/argo-secret-delivery.md); the store was seeded to satisfy that. A
# rename here MUST be paired with re-seeding the store fields, and vice versa
# — v0.13.1 failed exactly on this divergence.
r2_config_owned=false
registry_config_owned=false
tools_dir=${OBERTH_TOOLS_DIR:-/tmp/oberth-tools}
cosign_tool=$tools_dir/bin/cosign
helm_tool=$tools_dir/bin/helm
trivy_tool=$tools_dir/bin/trivy
release_support_tool=$tools_dir/bin/oberth-release-support
release_image_tool=$tools_dir/bin/oberth-release-image
go_tool=/usr/local/go/bin/go
runtime_receipt=${OBERTH_RUNTIME_RECEIPT:-/tmp/oberth-runtime-receipts/receipt}
release_public_key=.oberth/pins/release-cosign.pub
release_public_key_sha256=036dd71c9d3c07a19bff06d4392874014573f695620ac8554d9df175094c4a31
# Website publication (#649). Node and wrangler execute only from Pod-private
# scratch under $website_dir, written by the release-website leaf's own init
# containers from hash-checked inputs; the credential-free setup steps stage
# only those inputs (the pinned Node tarball and a content-addressed npm
# cache) in $website_inputs.
website_dir=${OBERTH_WEBSITE_DIR:-/tmp/oberth-website}
website_inputs=${OBERTH_WEBSITE_INPUTS:-/tmp/oberth-website-inputs}
website_node=$website_dir/node/bin/node
website_npm=$website_dir/node/lib/node_modules/npm/bin/npm-cli.js
website_app=$website_dir/app
website_wrangler=$website_app/node_modules/wrangler/bin/wrangler.js
website_site=$website_dir/site
website_node_pin=.oberth/pins/node.sha256
website_node_bin_pin=.oberth/pins/node-bin.sha256
# The oberth Cloudflare account (public identifier, not a secret).
website_account_id=0bc3ad9e8a1ef560fa5f1536d0696bc7
website_home_owned=false

fail() {
	printf 'release: %s\n' "$*" >&2
	exit 1
}

case "$tools_dir" in
	/*) ;;
	*) fail "OBERTH_TOOLS_DIR must be an absolute trusted tools directory" ;;
esac

# Ensure the credential scratch directory exists on the memory-backed volume.
# When OBERTH_SECRETSTORE_DIR is set (the real release pod), cred_dir MUST
# resolve under the store root -- falling back to node disk is a security
# violation (SIGKILL skips exit-trap cleanup, leaving credentials on the PVC).
ensure_cred_dir() {
	if [ -n "${OBERTH_SECRETSTORE_DIR:-}" ]; then
		case "$cred_dir" in
			"${OBERTH_SECRETSTORE_DIR}"/*) ;;
			*) fail "credential scratch must be under the memory-backed secret store mount (OBERTH_SECRETSTORE_DIR=$OBERTH_SECRETSTORE_DIR), got $cred_dir" ;;
		esac
	fi
	mkdir -p "$cred_dir" || fail "cannot create credential scratch directory $cred_dir -- is the memory-backed volume mounted?"
}

case "$release_dir" in
	/tmp/oberth-release|/tmp/oberth-release-*) ;;
	*) fail "OBERTH_RELEASE_DIR must be a dedicated /tmp/oberth-release path" ;;
esac
[ ! -L "$release_dir" ] || fail "release directory must not be a symlink"
case "$website_dir" in
	/tmp/oberth-website|/tmp/oberth-website-test-*) ;;
	*) fail "OBERTH_WEBSITE_DIR must be the Pod-private /tmp/oberth-website path" ;;
esac
case "$website_inputs" in
	/tmp/oberth-website-inputs|/tmp/oberth-website-inputs-test-*) ;;
	*) fail "OBERTH_WEBSITE_INPUTS must be the dedicated /tmp/oberth-website-inputs claim path" ;;
esac
case "$website_dir/$website_inputs/" in
	*/../*|*/./*) fail "website directories must be normalized absolute paths" ;;
esac
if [ -L "$website_dir" ] || [ -L "$website_inputs" ]; then
	fail "website directories must not be symlinks"
fi

# The release Job runs its steps as root while the /work/src checkout is
# prepared with a different owner; git's safe.directory protection therefore
# refuses every command in the checkout ("dubious ownership", exit 128).
# Declaring exactly this checkout safe is the intended remedy for a
# deliberately cross-owned repository. The mark is written only when git
# actually refuses the checkout — a local run on a self-owned clone changes
# nothing — and lives in the ephemeral Job container's own global config, so
# it never leaves the Pod. --replace-all keeps the entry single-valued across
# the several release.sh actions of one run.
if ! git rev-parse --git-dir >/dev/null 2>&1; then
	git config --global --replace-all safe.directory "$PWD" || \
		fail "could not mark the release checkout git-safe"
	git rev-parse --git-dir >/dev/null 2>&1 || \
		fail "release checkout is unusable even after the safe.directory mark"
fi

cleanup() {
	rm -f -- "$release_dir/cosign.pub.$$"
	if [ "$r2_config_owned" = true ]; then
		rm -f -- "$r2_curl_config"
	fi
	if [ "$registry_config_owned" = true ]; then
		rm -rf -- "$cred_dir/.docker" "$cred_dir/.helm"
		rm -f -- "$cred_dir/.gar-token"
	fi
	if [ "$website_home_owned" = true ]; then
		rm -rf -- "$cred_dir/.wrangler-home"
	fi
}
trap cleanup 0 1 2 15

validate_source() {
	printf '%s\n' "$tag" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$' || \
		fail "release tag must be a v-prefixed semantic version"
	printf '%s\n' "$sha" | grep -Eq '^[0-9a-f]{40}$' || fail "release SHA must be a lowercase 40-character commit ID"
	head_sha=$(git rev-parse --verify HEAD)
	[ "$head_sha" = "$sha" ] || fail "checked-out HEAD does not match the admitted release SHA"
	tag_sha=$(git rev-parse --verify "${tag}^{commit}")
	[ "$tag_sha" = "$sha" ] || fail "release tag does not peel to the admitted release SHA"
	git diff --quiet -- || fail "release checkout has tracked source modifications"
	git diff --cached --quiet -- || fail "release checkout has staged source modifications"
}

source_created() {
	git show -s --format=%cI "$sha"
}

require_artifacts() {
	for file in \
		oberth-linux-amd64 \
		oberth-linux-arm64 \
		oberth-darwin-amd64 \
		oberth-darwin-arm64 \
		SHA256SUMS; do
		test -s "$release_dir/$file" || fail "missing release artifact $file"
	done
	[ "$(wc -l <"$release_dir/SHA256SUMS" | tr -d ' ')" = 4 ] || fail "SHA256SUMS must bind exactly four executables"
	(cd "$release_dir" && sha256sum -c SHA256SUMS >/dev/null) || fail "local release artifact checksum mismatch"
}

native_binary() {
	case "$(uname -m)" in
		x86_64|amd64) printf '%s' oberth-linux-amd64 ;;
		aarch64|arm64) printf '%s' oberth-linux-arm64 ;;
		*) fail "unsupported release Job architecture $(uname -m)" ;;
	esac
}

require_tokenless_runtime() {
	[ ! -e /var/run/secrets/kubernetes.io/serviceaccount/token ] || fail "artifact execution requires a tokenless Pod"
	[ -z "${OBERTH_SECRETSTORE_DIR:-}${COSIGN_KEY:-}${COSIGN_PASSWORD:-}${VAULT_TOKEN:-}${GOOGLE_APPLICATION_CREDENTIALS:-}" ] || \
		fail "artifact execution refuses credential environment"
}

verify_binary_version() {
	require_tokenless_runtime
	binary=$1
	want_tag=$2
	want_sha=${3:-}
	chmod 0755 "$binary"
	version_output=$("$binary" version)
	if ! printf '%s\n' "$version_output" | awk -v tag="$want_tag" '
		NF == 4 && $1 == "oberth" && $2 == tag {
			commit = $3
			date = $4
			sub(/^commit=/, "", commit)
			sub(/^date=/, "", date)
			if (length(commit) == 12 && commit ~ /^[0-9a-f]+$/ && date ~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T/) ok = 1
		}
		END { exit ok ? 0 : 1 }
	'; then
		fail "release binary reports malformed version metadata"
	fi
	if [ -n "$want_sha" ]; then
		expected="oberth $want_tag commit=$(printf '%.12s' "$want_sha") date=$(source_created)"
		[ "$version_output" = "$expected" ] || fail "release binary reports the wrong source metadata"
	fi
}

build_artifacts() {
	validate_source
	command -v "$go_tool" >/dev/null 2>&1 || fail "Go is not installed"
	# release_dir is a mounted volume (work PVC, subPath release), not a plain
	# directory - rm -rf on the mount point itself fails "Device or resource
	# busy". Clear its contents instead, same end state without touching the
	# mount.
	if [ -e "$release_dir" ]; then
		find "$release_dir" -mindepth 1 -delete
	fi
	mkdir -p "$release_dir"
	source_epoch=$(git show -s --format=%ct "$sha")
	created=$(source_created)
	short_sha=$(printf '%.12s' "$sha")
	export SOURCE_DATE_EPOCH="$source_epoch"
	export CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off

	for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
		goos=${target%/*}
		goarch=${target#*/}
		output=oberth-${goos}-${goarch}
		GOOS=$goos GOARCH=$goarch "$go_tool" build \
			-trimpath -buildvcs=false \
			-ldflags="-s -w -buildid= -X main.version=${tag} -X main.commit=${short_sha} -X main.date=${created}" \
			-o "$release_dir/$output" ./cmd/oberth
		GOOS=$goos GOARCH=$goarch "$go_tool" build \
			-trimpath -buildvcs=false \
			-ldflags="-s -w -buildid= -X main.version=${tag} -X main.commit=${short_sha} -X main.date=${created}" \
			-o "$release_dir/$output.repro" ./cmd/oberth
		cmp -s "$release_dir/$output" "$release_dir/$output.repro" || fail "$output is not reproducible"
		rm -f -- "$release_dir/$output.repro"
		"$go_tool" version -m "$release_dir/$output" | grep -Eq '^[[:space:]]*path[[:space:]]+github\.com/oberthci/oberth/cmd/oberth$' || \
			fail "$output does not identify the Oberth command module"
	done

	(
		cd "$release_dir"
		sha256sum \
			oberth-linux-amd64 \
			oberth-linux-arm64 \
			oberth-darwin-amd64 \
			oberth-darwin-arm64 >SHA256SUMS
	)
	require_artifacts
	verify_binary_version "$release_dir/$(native_binary)" "$tag" "$sha"
}

public_object_status() {
	public_url=$1
	output=$2
	curl --silent --show-error --location \
		--proto '=https' --tlsv1.2 \
		--connect-timeout 10 --max-time 120 \
		--output "$output" --write-out '%{http_code}' \
		"$public_url" || true
}

initialize_authoritative_r2() {
	case "$r2_endpoint" in https://*) ;;
		*) fail "R2 S3 endpoint must use HTTPS" ;;
	esac
	mkdir -p "$release_dir"
	if [ -n "${OBERTH_R2_CURL_CONFIG:-}" ]; then
		test -f "$r2_curl_config" || fail "configured R2 curl config is missing"
		[ ! -L "$r2_curl_config" ] || fail "configured R2 curl config must not be a symlink"
		return
	fi
	ensure_cred_dir
	token_file=$secret_root/cloudflare-oberth-workers-token/CLOUDFLARE_API_TOKEN
	test -s "$token_file" || fail "cloudflare-oberth-workers-token/CLOUDFLARE_API_TOKEN is missing"
	command -v "$release_support_tool" >/dev/null 2>&1 || fail "oberth-release-support is not installed"
	"$release_support_tool" r2-auth "$token_file" "$r2_account" "$r2_bucket" oberth/ "$r2_curl_config"
	r2_config_owned=true
}

authoritative_object_status() {
	object_key=$1
	output=$2
	headers=$3
	if ! authoritative_status=$(curl --silent --show-error \
		--proto '=https' --tlsv1.2 --max-redirs 0 \
		--connect-timeout 10 --max-time 120 \
		--config "$r2_curl_config" \
		--dump-header "$headers" --output "$output" --write-out '%{http_code}' \
		"$r2_endpoint/$r2_bucket/$object_key"); then
		fail "authoritative R2 read failed for $object_key"
	fi
	printf '%s' "$authoritative_status"
}

put_conditionally() {
	object_key=$1
	file=$2
	content_type=$3
	condition=$4
	response_file=$release_dir/.r2-put-response
	if ! conditional_status=$(curl --silent --show-error \
		--proto '=https' --tlsv1.2 --max-redirs 0 \
		--connect-timeout 10 --max-time 300 \
		--config "$r2_curl_config" --output "$response_file" --write-out '%{http_code}' \
		--request PUT "$r2_endpoint/$r2_bucket/$object_key" \
		--header "$condition" --header "Content-Type: $content_type" \
		--data-binary "@$file"); then
		fail "conditional R2 write failed for $object_key"
	fi
	printf '%s' "$conditional_status"
}

wait_for_exact_public_object() {
	public_url=$1
	file=$2
	attempt=1
	while [ "$attempt" -le 24 ]; do
		downloaded=$release_dir/.public-object
		status=$(public_object_status "$public_url" "$downloaded")
		if [ "$status" = 200 ] && cmp -s "$file" "$downloaded"; then
			rm -f -- "$downloaded"
			return 0
		fi
		[ "$status" = 200 ] || [ "$status" = 404 ] || [ "$status" = 000 ] || fail "public object returned HTTP $status"
		rm -f -- "$downloaded"
		sleep 5
		attempt=$((attempt + 1))
	done
	fail "public object did not converge to the expected bytes: $public_url"
}

publish_exact_object() {
	object_key=$1
	file=$2
	content_type=$3
	public_url=$4
	put_status=$(put_conditionally "$object_key" "$file" "$content_type" 'If-None-Match: *')
	case "$put_status" in 200|201|409|412) ;;
		*) fail "immutable R2 create returned HTTP $put_status for $object_key" ;;
	esac
	authoritative=$release_dir/.authoritative-object
	headers=$release_dir/.authoritative-object.headers
	status=$(authoritative_object_status "$object_key" "$authoritative" "$headers")
	[ "$status" = 200 ] || fail "immutable R2 readback returned HTTP $status for $object_key"
	cmp -s "$file" "$authoritative" || fail "refusing to overwrite different immutable object $object_key after HTTP $put_status"
	rm -f -- "$authoritative" "$headers"
	wait_for_exact_public_object "$public_url" "$file"
}

prepare_signing_identity() {
	key_file=$secret_root/cosign-secret/COSIGN_KEY
	password_file=$secret_root/cosign-secret/COSIGN_PASSWORD
	expected_public_key=$secret_root/cosign-secret/COSIGN_PUB
	test -s "$key_file" || fail "cosign-secret/COSIGN_KEY is missing"
	command -v "$cosign_tool" >/dev/null 2>&1 || fail "cosign is not installed"
	unset COSIGN_KEY COSIGN_PASSWORD signing_key_file signing_password
	signing_key_file=$key_file
	signing_password=
	if [ -f "$password_file" ]; then
		signing_password=$(cat "$password_file")
	fi
	cosign_pub_tmp="$release_dir/cosign.pub.$$"
	COSIGN_PASSWORD=$signing_password COSIGN_YES=true "$cosign_tool" public-key --key "$signing_key_file" >"$cosign_pub_tmp"
	test -s "$cosign_pub_tmp" || { rm -f -- "$cosign_pub_tmp"; fail "cosign produced an empty public key"; }
	if [ -f "$expected_public_key" ]; then
		cmp -s "$expected_public_key" "$cosign_pub_tmp" || { rm -f -- "$cosign_pub_tmp"; fail "mounted cosign public key does not match its private key"; }
	fi
	verify_pinned_public_key "$cosign_pub_tmp"
	mv -f -- "$cosign_pub_tmp" "$release_dir/cosign.pub"
}

clear_signing_identity() {
	unset COSIGN_KEY COSIGN_PASSWORD signing_key_file signing_password
}

stage_signed_bundle() {
	payload=$1
	bundle=$2
	COSIGN_PASSWORD=$signing_password COSIGN_YES=true "$cosign_tool" sign-blob --use-signing-config=false --tlog-upload=false --key "$signing_key_file" --bundle "$bundle" "$payload" >/dev/null
	test -s "$bundle" || fail "cosign produced an empty signature bundle"
	"$cosign_tool" verify-blob --key "$release_dir/cosign.pub" --insecure-ignore-tlog --bundle "$bundle" "$payload" >/dev/null || \
		fail "staged signature bundle does not authenticate its payload"
}

publish_signed_bundle() {
	object_key=$1
	public_url=$2
	payload=$3
	bundle=$4
	existing=$release_dir/.authoritative-bundle
	existing_headers=$release_dir/.authoritative-bundle.headers
	bundle_status=$(authoritative_object_status "$object_key" "$existing" "$existing_headers")
	case "$bundle_status" in
		200)
			mv "$existing" "$bundle"
			"$cosign_tool" verify-blob --key "$release_dir/cosign.pub" --insecure-ignore-tlog --bundle "$bundle" "$payload" >/dev/null || \
				fail "existing immutable signature bundle does not authenticate its payload"
			;;
		404)
			rm -f -- "$existing" "$existing_headers"
			test -s "$bundle" || fail "staged signature bundle is missing"
			"$cosign_tool" verify-blob --key "$release_dir/cosign.pub" --insecure-ignore-tlog --bundle "$bundle" "$payload" >/dev/null || \
				fail "new signature bundle does not authenticate its payload"
			put_status=$(put_conditionally "$object_key" "$bundle" application/json 'If-None-Match: *')
			case "$put_status" in 200|201|409|412) ;;
				*) fail "immutable signature create returned HTTP $put_status" ;;
			esac
			readback=$release_dir/.authoritative-bundle
			readback_headers=$release_dir/.authoritative-bundle.headers
			readback_status=$(authoritative_object_status "$object_key" "$readback" "$readback_headers")
			[ "$readback_status" = 200 ] || fail "immutable signature readback returned HTTP $readback_status"
			"$cosign_tool" verify-blob --key "$release_dir/cosign.pub" --insecure-ignore-tlog --bundle "$readback" "$payload" >/dev/null || \
				fail "authoritative signature bundle does not authenticate its payload after HTTP $put_status"
			mv "$readback" "$bundle"
			rm -f -- "$readback_headers"
			;;
		*) fail "immutable signature precondition returned HTTP $bundle_status" ;;
	esac
	rm -f -- "$existing_headers"
	wait_for_exact_public_object "$public_url" "$bundle"
}

sign_binaries() {
	validate_source
	require_artifacts
	prepare_signing_identity
	stage_signed_bundle "$release_dir/SHA256SUMS" "$release_dir/SHA256SUMS.sigstore.json"
	clear_signing_identity
}

publish_public_release() {
	validate_source
	require_artifacts
	require_chart
	verify_pinned_public_key "$release_dir/cosign.pub"
	for payload in SHA256SUMS "oberth-${tag#v}.tgz" release.json; do
		"$cosign_tool" verify-blob --key "$release_dir/cosign.pub" --insecure-ignore-tlog \
			--bundle "$release_dir/$payload.sigstore.json" "$release_dir/$payload" >/dev/null || \
			fail "public publication requires a valid staged signature"
	done
	initialize_authoritative_r2
	for file in oberth-linux-amd64 oberth-linux-arm64 oberth-darwin-amd64 oberth-darwin-arm64 SHA256SUMS cosign.pub release.json; do
		publish_exact_object "oberth/$tag/$file" "$release_dir/$file" "$(artifact_content_type "$file")" "$release_origin/oberth/$tag/$file"
	done
	for payload in SHA256SUMS release.json; do
		publish_signed_bundle "oberth/$tag/$payload.sigstore.json" "$release_origin/oberth/$tag/$payload.sigstore.json" \
			"$release_dir/$payload" "$release_dir/$payload.sigstore.json"
	done
	chart_name=oberth-${tag#v}.tgz
	publish_exact_object "oberth/$chart_name" "$release_dir/$chart_name" application/gzip "$chart_repo/$chart_name"
	publish_exact_object "oberth/cosign.pub" "$release_dir/cosign.pub" 'text/plain; charset=utf-8' "$chart_repo/cosign.pub"
	publish_signed_bundle "oberth/$chart_name.sigstore.json" "$chart_repo/$chart_name.sigstore.json" \
		"$release_dir/$chart_name" "$release_dir/$chart_name.sigstore.json"
}

# Snapshot signed public inputs into this Pod's scratch. The publisher never
# writes the shared candidate claim, and no candidate executable runs here.
stage_public_inputs() {
	mkdir -p "$release_dir"
	/usr/bin/python3 -I -S -B - "$tag" "$release_dir" <<'PY' || fail "cannot snapshot signed public inputs"
import os
import stat
import sys

tag, destination = sys.argv[1:]
files = {
    "oberth-linux-amd64": 512 << 20,
    "oberth-linux-arm64": 512 << 20,
    "oberth-darwin-amd64": 512 << 20,
    "oberth-darwin-arm64": 512 << 20,
    "SHA256SUMS": 8192,
    "SHA256SUMS.sigstore.json": 1 << 20,
    "cosign.pub": 8192,
    "release.json": 65536,
    "release.json.sigstore.json": 1 << 20,
    "server-image.txt": 1024,
    "chart-image.txt": 1024,
    "chart-sha256.txt": 1024,
    "oberth-" + tag[1:] + ".tgz": 16 << 20,
    "oberth-" + tag[1:] + ".tgz.sigstore.json": 1 << 20,
}
root = os.open("/tmp/oberth-release-inputs", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
target = os.open(destination, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
try:
    for name, limit in files.items():
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=root)
        with os.fdopen(fd, "rb") as source:
            before = os.fstat(source.fileno())
            if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or not 0 < before.st_size <= limit:
                raise ValueError("invalid public input")
            out = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600, dir_fd=target)
            with os.fdopen(out, "wb") as output:
                remaining = before.st_size
                while remaining:
                    body = source.read(min(1 << 20, remaining))
                    if not body:
                        raise ValueError("truncated public input")
                    output.write(body)
                    remaining -= len(body)
                after = os.fstat(source.fileno())
                if source.read(1) or (before.st_size, before.st_mtime_ns, before.st_ctime_ns) != (after.st_size, after.st_mtime_ns, after.st_ctime_ns):
                    raise ValueError("changed public input")
finally:
    os.close(root)
    os.close(target)
PY
}

cloudflare_phase() {
	# Repository-owned DAG arguments are still explicitly closed: this token
	# must never become an arbitrary release-command or artifact-execution hook.
	case "$1" in
		publish-public|finalize|publish-website) ;;
		*) fail "unsupported Cloudflare publication phase" ;;
	esac
	validate_source
	case "$1" in
		publish-public) stage_public_inputs; publish_public_release ;;
		finalize) stage_public_inputs; finalize_release ;;
		publish-website) publish_website ;;
	esac
}

publish_homebrew_tap() {
	validate_source
	require_artifacts
	tap_key=$secret_root/homebrew-tap-key/SSH_KEY
	test -s "$tap_key" || fail "homebrew-tap-key/SSH_KEY is missing"
	tap_dir=$release_dir/.homebrew-tap
	ensure_cred_dir
	tap_ssh=$cred_dir/.homebrew-tap-ssh
	tap_known_hosts=$release_dir/.homebrew-tap-known-hosts
	rm -rf -- "$tap_dir" "$tap_ssh" "$tap_known_hosts"
	mkdir -m 0700 "$tap_ssh"
	cp "$tap_key" "$tap_ssh/id"
	chmod 0600 "$tap_ssh/id"

	# ssh.github.com:443 serves the same SSH host key as github.com:22 (per
	# GitHub's documentation) and is the only path the pipeline NetworkPolicy
	# allows (egress limited to ports 53, 443, 8200). Pin the ed25519
	# fingerprint the same way upstream_bootstrap.go's wellKnownForges does.
	scanned=$(ssh-keyscan -t ed25519 -p 443 ssh.github.com 2>/dev/null) || fail "could not scan ssh.github.com:443 host key"
	fingerprint=$(printf '%s\n' "$scanned" | ssh-keygen -lf - | awk '{print $2}')
	[ "$fingerprint" = "SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU" ] || \
		fail "ssh.github.com host key fingerprint does not match the pinned value ($fingerprint)"
	printf '%s\n' "$scanned" >"$tap_known_hosts"

	export GIT_SSH_COMMAND="ssh -i $tap_ssh/id -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$tap_known_hosts"
	git clone --depth 1 ssh://git@ssh.github.com:443/oberthci/homebrew-tap.git "$tap_dir" >/dev/null

	formula=$tap_dir/Formula/oberth.rb
	test -f "$formula" || fail "homebrew-tap has no Formula/oberth.rb"

	darwin_amd64=$(awk '/oberth-darwin-amd64/{print $1}' "$release_dir/SHA256SUMS")
	darwin_arm64=$(awk '/oberth-darwin-arm64/{print $1}' "$release_dir/SHA256SUMS")
	linux_amd64=$(awk '/oberth-linux-amd64/{print $1}' "$release_dir/SHA256SUMS")
	linux_arm64=$(awk '/oberth-linux-arm64/{print $1}' "$release_dir/SHA256SUMS")
	for value in "$darwin_amd64" "$darwin_arm64" "$linux_amd64" "$linux_arm64"; do
		printf '%s\n' "$value" | grep -Eq '^[0-9a-f]{64}$' || fail "SHA256SUMS produced a malformed digest"
	done

	cat >"$formula" <<-EOF
	class Oberth < Formula
	  desc "Single-node Git-over-SSH CI service for Kubernetes with repository-owned Go pipelines"
	  homepage "https://oberth.ci"
	  version "${tag#v}"
	  license "Proprietary"

	  on_macos do
	    on_intel do
	      url "$release_origin/oberth/$tag/oberth-darwin-amd64"
	      sha256 "$darwin_amd64"
	    end
	    on_arm do
	      url "$release_origin/oberth/$tag/oberth-darwin-arm64"
	      sha256 "$darwin_arm64"
	    end
	  end

	  on_linux do
	    on_intel do
	      url "$release_origin/oberth/$tag/oberth-linux-amd64"
	      sha256 "$linux_amd64"
	    end
	    on_arm do
	      url "$release_origin/oberth/$tag/oberth-linux-arm64"
	      sha256 "$linux_arm64"
	    end
	  end

	  def install
	    binary = stable.url.split("/").last
	    bin.install binary => "oberth"
	  end

	  test do
	    system bin/"oberth", "version"
	  end
	end
	EOF

	if git -C "$tap_dir" diff --quiet -- Formula/oberth.rb; then
		printf 'release: homebrew-tap Formula already at %s, nothing to push\n' "$tag" >&2
	else
		git -C "$tap_dir" -c user.name="oberth-release" -c user.email="release@oberth.ci" \
			add Formula/oberth.rb
		git -C "$tap_dir" -c user.name="oberth-release" -c user.email="release@oberth.ci" \
			commit -q -m "cleanup release $tag"
		git -C "$tap_dir" push -q origin HEAD:main
	fi

	verify_dir=$release_dir/.homebrew-tap-verify
	rm -rf -- "$verify_dir"
	git clone --depth 1 ssh://git@ssh.github.com:443/oberthci/homebrew-tap.git "$verify_dir" >/dev/null
	grep -q "version \"${tag#v}\"" "$verify_dir/Formula/oberth.rb" || \
		fail "homebrew-tap Formula does not show $tag after push"
	grep -q "$darwin_amd64" "$verify_dir/Formula/oberth.rb" || \
		fail "homebrew-tap Formula does not show the darwin-amd64 digest after push"

	rm -rf -- "$tap_dir" "$tap_ssh" "$tap_known_hosts" "$verify_dir"
}

prepare_registry_auth() {
	# Each leaf retains its original source credential. IAM permits only its
	# matching fixed Oberth principal; no new long-lived key is created.
	case "$action" in
		publish-images) gar_source=$secret_root/gar-image-key/GAR_SA_KEY ;;
		publish-chart) gar_source=$secret_root/gar-chart-key/GAR_SA_KEY ;;
		verify) gar_source=$secret_root/gar-reader-key/GAR_SA_KEY ;;
		*) fail "GAR credential requested by an unsupported release action" ;;
	esac
	ensure_cred_dir
	registry_config_owned=true
	gar_key=$cred_dir/.gar-token
	rm -f -- "$gar_key"
	"$release_support_tool" registry-token "$action" "$gar_source" "$gar_key" || fail "GAR role delegation failed"
	test -s "$gar_key" || fail "GAR role delegation returned no access token"
	rm -rf -- "$cred_dir/.docker" "$cred_dir/.helm"
	mkdir -p "$cred_dir/.docker" "$cred_dir/.helm/registry"
	auth=$( (printf '%s' 'oauth2accesstoken:'; cat "$gar_key") | base64 | tr -d '\n')
	printf '{"auths":{"%s":{"auth":"%s"}}}\n' "$gar_host" "$auth" >"$cred_dir/.docker/config.json"
	chmod 0600 "$cred_dir/.docker/config.json"
	DOCKER_CONFIG=$cred_dir/.docker
	HELM_REGISTRY_CONFIG=$cred_dir/.helm/registry/config.json
	export DOCKER_CONFIG HELM_REGISTRY_CONFIG
	registry_config_owned=true
}

sign_registry_object() {
	reference=$1
	if ! "$cosign_tool" verify --key "$release_dir/cosign.pub" --insecure-ignore-tlog "$reference" >/dev/null 2>&1; then
		COSIGN_PASSWORD=$signing_password COSIGN_YES=true "$cosign_tool" sign --use-signing-config=false --tlog-upload=false --key "$signing_key_file" "$reference" >/dev/null
	fi
	"$cosign_tool" verify --key "$release_dir/cosign.pub" --insecure-ignore-tlog "$reference" >/dev/null || fail "registry signature verification failed for $reference"
}

scan_image() {
	reference=$1
	"$trivy_tool" image \
		--cache-dir /tmp/oberth-trivy \
		--skip-check-update --skip-vex-repo-update --skip-version-check \
		--disable-telemetry --scanners vuln \
		--exit-code 1 --severity HIGH,CRITICAL "$reference"
}

publish_images() {
	validate_source
	require_artifacts
	command -v "$release_image_tool" >/dev/null 2>&1 || fail "oberth-release-image is not installed"
	command -v "$trivy_tool" >/dev/null 2>&1 || fail "Trivy is not installed"
	prepare_registry_auth
	created=$(source_created)
	"$release_image_tool" publish "$tag" "$sha" "$created" "$release_dir" "$gar_key"
	reference=$(sed -n '1p' "$release_dir/server-image.txt")
	[ -n "$reference" ] || fail "release image helper produced an empty reference"
	scan_image "$reference"
	prepare_signing_identity
	sign_registry_object "$reference"
	clear_signing_identity
	"$release_image_tool" verify "$tag" "$sha" "$created" "$release_dir" "$gar_key"
}

prepare_chart() {
	validate_source
	require_artifacts
	command -v "$helm_tool" >/dev/null 2>&1 || fail "Helm is not installed"
	command -v "$release_support_tool" >/dev/null 2>&1 || fail "oberth-release-support is not installed"
	server_ref=$(sed -n '1p' "$release_dir/server-image.txt")
	case "$server_ref" in ${gar_host}/skipopsmain/oberth/oberth@sha256:*) ;;
		*) fail "server image reference is not an immutable Oberth GAR digest" ;;
	esac
	chart_source=$release_dir/chart-source
	rm -rf -- "$chart_source" "$release_dir/chart-a" "$release_dir/chart-b"
	cp -R charts/oberth "$chart_source"
	if ! awk -v server="$server_ref" '
		/^image:$/ { section = "server"; print; next }
		/^[^[:space:]]/ { section = "" }
		section == "server" && $1 == "ref:" { print "  ref: " server; servers++; next }
		{ print }
		END { if (servers != 1) exit 42 }
	' "$chart_source/values.yaml" >"$chart_source/values.yaml.new"; then
		fail "chart values do not contain exactly one server image reference"
	fi
	mv "$chart_source/values.yaml.new" "$chart_source/values.yaml"
	created=$(source_created)
	"$release_support_tool" normalize-tree "$chart_source" "$created"
	"$helm_tool" lint "$chart_source"
	"$helm_tool" template oberth "$chart_source" --namespace oberth >"$release_dir/chart-rendered.yaml"
	grep -Fq "$server_ref" "$release_dir/chart-rendered.yaml" || fail "release chart does not render the exact server digest"
	chart_version=${tag#v}
	mkdir -p "$release_dir/chart-a" "$release_dir/chart-b"
	"$helm_tool" package "$chart_source" --version "$chart_version" --app-version "$tag" --destination "$release_dir/chart-a" >/dev/null
	"$helm_tool" package "$chart_source" --version "$chart_version" --app-version "$tag" --destination "$release_dir/chart-b" >/dev/null
	chart_name=oberth-${chart_version}.tgz
	cmp -s "$release_dir/chart-a/$chart_name" "$release_dir/chart-b/$chart_name" || fail "Oberth chart package is not reproducible"
	cp "$release_dir/chart-a/$chart_name" "$release_dir/$chart_name"
	chart_sha=$(sha256sum "$release_dir/$chart_name" | cut -d ' ' -f 1)
	printf 'sha256:%s\n' "$chart_sha" >"$release_dir/chart-sha256.txt"
	git diff --quiet -- charts/oberth || fail "release chart preparation modified tracked source"
}

require_chart() {
	chart_name=oberth-${tag#v}.tgz
	test -s "$release_dir/$chart_name" || fail "missing release chart $chart_name"
	test -s "$release_dir/chart-sha256.txt" || fail "missing release chart checksum"
	want_chart_digest=$(sed -n '1p' "$release_dir/chart-sha256.txt")
	actual_chart_digest=sha256:$(sha256sum "$release_dir/$chart_name" | cut -d ' ' -f 1)
	[ "$want_chart_digest" = "$actual_chart_digest" ] || fail "release chart checksum mismatch"
}

pull_gar_chart() {
	destination=$1
	rm -rf -- "$destination"
	mkdir -p "$destination"
	"$helm_tool" pull "$gar_chart_oci/oberth" --version "${tag#v}" --destination "$destination" >/dev/null
}

generate_release_json() {
	validate_source
	require_artifacts
	require_chart
	test -s "$release_dir/server-image.txt" || fail "server-image.txt is required for release.json"
	test -s "$release_dir/chart-sha256.txt" || fail "chart-sha256.txt is required for release.json"
	server_ref=$(sed -n '1p' "$release_dir/server-image.txt")
	chart_version=${tag#v}
	chart_digest=$(sed -n '1p' "$release_dir/chart-sha256.txt")
	# Read binary digests from SHA256SUMS.
	linux_amd64=$(awk '/oberth-linux-amd64/{print $1}' "$release_dir/SHA256SUMS")
	linux_arm64=$(awk '/oberth-linux-arm64/{print $1}' "$release_dir/SHA256SUMS")
	darwin_amd64=$(awk '/oberth-darwin-amd64/{print $1}' "$release_dir/SHA256SUMS")
	darwin_arm64=$(awk '/oberth-darwin-arm64/{print $1}' "$release_dir/SHA256SUMS")
	for digest in "$linux_amd64" "$linux_arm64" "$darwin_amd64" "$darwin_arm64"; do
		printf '%s\n' "$digest" | grep -Eq '^[0-9a-f]{64}$' || fail "SHA256SUMS produced a malformed digest for release.json"
	done
	# Read chart GAR reference if available.
	chart_gar_ref=""
	if [ -f "$release_dir/chart-image.txt" ]; then
		chart_gar_ref=$(sed -n '1p' "$release_dir/chart-image.txt")
	fi
	created=$(source_created)
	/usr/bin/python3 -I -S -B -c '
import json, sys
tag, sha, created = sys.argv[1], sys.argv[2], sys.argv[3]
server_ref = sys.argv[4]
chart_version, chart_digest = sys.argv[5], sys.argv[6]
chart_gar_ref = sys.argv[7] if len(sys.argv) > 7 and sys.argv[7] else None
digests = dict(zip(
    ["oberth-linux-amd64","oberth-linux-arm64","oberth-darwin-amd64","oberth-darwin-arm64"],
    sys.argv[8:12]
))
record = {
    "schemaVersion": 1,
    "component": "oberth",
    "version": tag,
    "source": {"repository": "github.com/oberthci/oberth", "sha": sha, "created": created},
    "artifacts": {"sha256:" + v: k for k, v in digests.items()},
    "images": {"server": {"ref": server_ref}},
    "chart": {"version": chart_version, "digest": chart_digest},
}
for k, v in digests.items():
    record["artifacts"][k] = "sha256:" + v
if chart_gar_ref:
    record["chart"]["garRef"] = chart_gar_ref
with open(sys.argv[12], "w") as f:
    json.dump(record, f, indent=2, sort_keys=True)
    f.write("\n")
' "$tag" "$sha" "$created" "$server_ref" "$chart_version" "$chart_digest" \
		"$chart_gar_ref" "$linux_amd64" "$linux_arm64" "$darwin_amd64" "$darwin_arm64" \
		"$release_dir/release.json" || fail "release.json generation failed"
	test -s "$release_dir/release.json" || fail "release.json is empty"
	# Validate the generated JSON is well-formed and contains the tag.
	/usr/bin/python3 -I -S -B -c '
import json, sys
with open(sys.argv[1]) as f:
    record = json.load(f)
assert record["version"] == sys.argv[2], "version mismatch"
assert record["source"]["sha"] == sys.argv[3], "sha mismatch"
assert record["component"] == "oberth", "component mismatch"
assert record["schemaVersion"] == 1, "schema version mismatch"
' "$release_dir/release.json" "$tag" "$sha" || fail "release.json validation failed"
}

publish_chart() {
	validate_source
	require_artifacts
	require_chart
	prepare_registry_auth
	"$helm_tool" registry login "$gar_host" --username oauth2accesstoken --password-stdin <"$gar_key" >/dev/null
	chart_version=${tag#v}
	chart_tag=$(printf '%s' "$chart_version" | tr '+' '_')
	chart_name=oberth-${chart_version}.tgz
	chart_path=$release_dir/$chart_name
	tag_reference=$gar_chart:$chart_tag
	registry_state=$("$release_support_tool" registry-digest "$tag_reference" "$gar_key")
	if [ "$registry_state" = missing ]; then
		if "$helm_tool" push "$chart_path" "$gar_chart_oci" >/dev/null; then
			:
		else
			push_status=$?
			registry_state=$("$release_support_tool" registry-digest "$tag_reference" "$gar_key")
			[ "$registry_state" != missing ] || fail "GAR chart push failed with status $push_status and no exact object appeared"
		fi
	fi
	pull_gar_chart "$release_dir/gar-chart"
	cmp -s "$chart_path" "$release_dir/gar-chart/$chart_name" || fail "GAR chart version already exists with different bytes"
	chart_digest=$("$release_support_tool" registry-digest "$tag_reference" "$gar_key")
	printf '%s\n' "$chart_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || fail "GAR did not return an immutable chart digest"
	chart_reference=$gar_chart@$chart_digest
	printf '%s\n' "$chart_reference" >"$release_dir/chart-image.txt"
	prepare_signing_identity
	sign_registry_object "$chart_reference"
	stage_signed_bundle "$chart_path" "$release_dir/$chart_name.sigstore.json"
	generate_release_json
	stage_signed_bundle "$release_dir/release.json" "$release_dir/release.json.sigstore.json"
	clear_signing_identity
}

download_public_object() {
	public_url=$1
	output=$2
	attempt=1
	while [ "$attempt" -le 24 ]; do
		if curl --fail --silent --show-error --location \
			--proto '=https' --tlsv1.2 \
			--connect-timeout 10 --max-time 120 \
			--output "$output" "$public_url"; then
			return 0
		fi
		sleep 5
		attempt=$((attempt + 1))
	done
	fail "could not download public release object $public_url"
}

verify_public_binaries() {
	verify_dir=$release_dir/verified-binaries
	rm -rf -- "$verify_dir"
	mkdir -p "$verify_dir"
	for file in \
		oberth-linux-amd64 \
		oberth-linux-arm64 \
		oberth-darwin-amd64 \
		oberth-darwin-arm64 \
		SHA256SUMS \
		SHA256SUMS.sigstore.json \
		cosign.pub \
		release.json \
		release.json.sigstore.json; do
		download_public_object "$release_origin/oberth/$tag/$file" "$verify_dir/$file"
		cmp -s "$release_dir/$file" "$verify_dir/$file" || fail "downloaded $file differs from the published candidate"
	done
	(cd "$verify_dir" && sha256sum -c SHA256SUMS >/dev/null) || fail "downloaded artifact checksum mismatch"
	"$cosign_tool" verify-blob --key "$verify_dir/cosign.pub" --insecure-ignore-tlog --bundle "$verify_dir/SHA256SUMS.sigstore.json" "$verify_dir/SHA256SUMS" >/dev/null || \
		fail "downloaded binary signature bundle is invalid"
	"$cosign_tool" verify-blob --key "$verify_dir/cosign.pub" --insecure-ignore-tlog --bundle "$verify_dir/release.json.sigstore.json" "$verify_dir/release.json" >/dev/null || \
		fail "downloaded release.json signature bundle is invalid"
	# Cross-check: release.json server image ref must match the published one.
	/usr/bin/python3 -I -S -B -c '
import json, sys
with open(sys.argv[1]) as f:
    record = json.load(f)
with open(sys.argv[2]) as f:
    server_ref = f.read().strip().split("\n")[0]
assert record["images"]["server"]["ref"] == server_ref, \
    "release.json server image ref does not match server-image.txt"
assert record["version"] == sys.argv[3], "release.json version mismatch"
assert record["source"]["sha"] == sys.argv[4], "release.json sha mismatch"
' "$verify_dir/release.json" "$release_dir/server-image.txt" "$tag" "$sha" || \
		fail "release.json cross-check failed"
}

verify_pinned_public_key() {
	if [ ! -f "$release_public_key" ] || [ -L "$release_public_key" ]; then
		fail "release public key pin is missing or unsafe"
	fi
	actual_key_hash=$(sha256sum "$release_public_key" | cut -d ' ' -f 1)
	[ "$actual_key_hash" = "$release_public_key_sha256" ] || fail "release public key pin changed"
	cmp -s "$release_public_key" "$1" || fail "release signing key does not match the source pin"
}

verify_runtime_payload() {
	verify_pinned_public_key "$release_dir/cosign.pub"
	# Bound checksum paths before sha256sum reads anything from a download.
	awk '
		NF != 2 || length($1) != 64 || $1 !~ /^[0-9a-f]+$/ { exit 1 }
		$2 !~ /^oberth-(linux|darwin)-(amd64|arm64)$/ || seen[$2]++ { exit 1 }
		END { if (NR != 4) exit 1 }
	' "$release_dir/SHA256SUMS" || fail "downloaded checksums must name exactly four release binaries"
	(cd "$release_dir" && sha256sum -c SHA256SUMS >/dev/null) || fail "downloaded artifact checksum mismatch"
	"$cosign_tool" verify-blob --key "$release_public_key" --insecure-ignore-tlog --bundle "$release_dir/SHA256SUMS.sigstore.json" "$release_dir/SHA256SUMS" >/dev/null || \
		fail "downloaded binary signature bundle is invalid"
}

runtime_receipt_bytes() {
	printf 'schema=oberth-public-runtime-v1\ntag=%s\nsha=%s\n' "$tag" "$sha"
	printf 'checksums=%s\n' "$(sha256sum "$release_dir/SHA256SUMS" | cut -d ' ' -f 1)"
	printf 'native=%s\n' "$(native_binary)"
	printf 'binary=%s\n' "$(sha256sum "$release_dir/$(native_binary)" | cut -d ' ' -f 1)"
	printf 'key=%s\n' "$(sha256sum "$release_dir/cosign.pub" | cut -d ' ' -f 1)"
}

verify_public_runtime() {
	validate_source
	require_tokenless_runtime
	# The workflow gives this leaf private scratch and a dedicated receipt
	# output only; neither publisher tools nor release outputs are mounted.
	mkdir -p "$release_dir" "$(dirname "$runtime_receipt")"
	[ ! -L "$runtime_receipt" ] || fail "runtime receipt must not be a symlink"
	rm -f -- "$runtime_receipt"
	for file in \
		oberth-linux-amd64 \
		oberth-linux-arm64 \
		oberth-darwin-amd64 \
		oberth-darwin-arm64 \
		SHA256SUMS \
		SHA256SUMS.sigstore.json \
		cosign.pub; do
		download_public_object "$release_origin/oberth/$tag/$file" "$release_dir/$file"
	done
	verify_runtime_payload
	# Keep the expected acceptance record private until the command succeeds.
	expected_receipt=$(runtime_receipt_bytes)
	verify_binary_version "$release_dir/$(native_binary)" "$tag" "$sha"
	# The artifact executed arbitrary code. Recheck all inputs and the exact
	# record after it returns before accepting the verification result.
	verify_runtime_payload
	[ "$(runtime_receipt_bytes)" = "$expected_receipt" ] || fail "runtime verification inputs changed during execution"
	[ ! -L "$runtime_receipt" ] || fail "runtime receipt must not be a symlink"
	printf '%s\n' "$expected_receipt" >"$runtime_receipt"
}

require_runtime_receipt() {
	if [ ! -f "$runtime_receipt" ] || [ -L "$runtime_receipt" ]; then
		fail "matching tokenless runtime receipt is required before finalization"
	fi
	verify_pinned_public_key "$release_dir/cosign.pub"
	expected_receipt_file=$release_dir/.expected-runtime-receipt
	runtime_receipt_bytes >"$expected_receipt_file"
	if ! cmp -s "$expected_receipt_file" "$runtime_receipt"; then
		rm -f -- "$expected_receipt_file"
		fail "runtime receipt does not bind this release source and artifact bytes"
	fi
	rm -f -- "$expected_receipt_file"
}

verify_chart() {
	require_chart
	chart_version=${tag#v}
	chart_name=oberth-${chart_version}.tgz
	chart_path=$release_dir/$chart_name
	test -s "$release_dir/chart-image.txt" || fail "missing immutable GAR chart reference"
	chart_reference=$(sed -n '1p' "$release_dir/chart-image.txt")
	case "$chart_reference" in ${gar_chart}@sha256:*) ;;
		*) fail "stored GAR chart reference is malformed" ;;
	esac
	pull_gar_chart "$release_dir/verified-gar-chart"
	cmp -s "$chart_path" "$release_dir/verified-gar-chart/$chart_name" || fail "downloaded GAR chart differs from the source package"
	chart_tag=$(printf '%s' "$chart_version" | tr '+' '_')
	resolved=$("$release_support_tool" registry-digest "$gar_chart:$chart_tag" "$gar_key")
	[ "$gar_chart@$resolved" = "$chart_reference" ] || fail "GAR chart tag does not resolve to the signed digest"
	"$cosign_tool" verify --key "$release_dir/cosign.pub" --insecure-ignore-tlog "$chart_reference" >/dev/null || fail "GAR chart signature is invalid"
	public_dir=$release_dir/verified-public-chart
	rm -rf -- "$public_dir"
	mkdir -p "$public_dir"
	download_public_object "$chart_repo/$chart_name" "$public_dir/$chart_name"
	download_public_object "$chart_repo/$chart_name.sigstore.json" "$public_dir/$chart_name.sigstore.json"
	download_public_object "$chart_repo/cosign.pub" "$public_dir/cosign.pub"
	cmp -s "$chart_path" "$public_dir/$chart_name" || fail "public chart differs from the source package"
	cmp -s "$release_dir/cosign.pub" "$public_dir/cosign.pub" || fail "public chart signing key differs from the mounted release key"
	"$cosign_tool" verify-blob --key "$public_dir/cosign.pub" --insecure-ignore-tlog --bundle "$public_dir/$chart_name.sigstore.json" "$public_dir/$chart_name" >/dev/null || \
		fail "public chart signature bundle is invalid"
	"$helm_tool" template oberth "$public_dir/$chart_name" --namespace oberth >"$public_dir/rendered.yaml"
	grep -Fq "$(sed -n '1p' "$release_dir/server-image.txt")" "$public_dir/rendered.yaml" || fail "public chart does not render the signed server image"
}

verify_release() {
	validate_source
	require_artifacts
	command -v "$cosign_tool" >/dev/null 2>&1 || fail "cosign is not installed"
	command -v "$trivy_tool" >/dev/null 2>&1 || fail "Trivy is not installed"
	command -v "$helm_tool" >/dev/null 2>&1 || fail "Helm is not installed"
	command -v "$release_image_tool" >/dev/null 2>&1 || fail "oberth-release-image is not installed"
	verify_pinned_public_key "$release_dir/cosign.pub"
	prepare_registry_auth
	verify_public_binaries
	created=$(source_created)
	"$release_image_tool" verify "$tag" "$sha" "$created" "$release_dir" "$gar_key"
	reference=$(sed -n '1p' "$release_dir/server-image.txt")
	"$cosign_tool" verify --key "$release_dir/cosign.pub" --insecure-ignore-tlog "$reference" >/dev/null || fail "GAR image signature is invalid"
	scan_image "$reference"
	verify_chart
}

artifact_content_type() {
	case "$1" in
		*.json) printf '%s' application/json ;;
		*.pub|SHA256SUMS|VERSION) printf '%s' 'text/plain; charset=utf-8' ;;
		*) printf '%s' application/octet-stream ;;
	esac
}

extract_etag() {
	awk 'tolower($1) == "etag:" { sub(/\r$/, "", $2); value = $2 } END { print value }' "$1"
}

put_mutable_exact() (
	mutable_key=$1
	mutable_file=$2
	mutable_type=$3
	mutable_attempt=1
	while [ "$mutable_attempt" -le 10 ]; do
		mutable_current=$release_dir/.mutable-current
		mutable_headers=$release_dir/.mutable-current.headers
		mutable_status=$(authoritative_object_status "$mutable_key" "$mutable_current" "$mutable_headers")
		case "$mutable_status" in
			200)
				if cmp -s "$mutable_file" "$mutable_current"; then
					rm -f -- "$mutable_current" "$mutable_headers"
					return 0
				fi
				mutable_etag=$(extract_etag "$mutable_headers")
				case "$mutable_etag" in \"*\") mutable_condition="If-Match: $mutable_etag" ;;
					*) fail "R2 returned an invalid ETag for $mutable_key" ;;
				esac
				;;
			404) mutable_condition='If-None-Match: *' ;;
			*) fail "mutable R2 precondition returned HTTP $mutable_status for $mutable_key" ;;
		esac
		mutable_put=$(put_conditionally "$mutable_key" "$mutable_file" "$mutable_type" "$mutable_condition")
		case "$mutable_put" in 200|201) ;;
			409|412) sleep 1 ;;
			*) fail "mutable R2 write returned HTTP $mutable_put for $mutable_key" ;;
		esac
		mutable_attempt=$((mutable_attempt + 1))
	done
	fail "mutable R2 object $mutable_key did not converge after concurrent updates"
)

read_marker_version() (
	marker_file=$1
	marker_version=$(sed -n '1p' "$marker_file")
	printf '%s\n' "$marker_version" >"$release_dir/.normalized-version"
	cmp -s "$marker_file" "$release_dir/.normalized-version" || fail "latest VERSION has non-canonical bytes"
	"$release_support_tool" semver-compare "$marker_version" "$marker_version" >/dev/null || fail "latest VERSION is not semantic"
	printf '%s' "$marker_version"
)

advance_latest_marker() (
	printf '%s\n' "$tag" >"$release_dir/VERSION"
	marker_attempt=1
	while [ "$marker_attempt" -le 10 ]; do
		marker_current=$release_dir/.latest-VERSION
		marker_headers=$release_dir/.latest-VERSION.headers
		marker_status=$(authoritative_object_status oberth/latest/VERSION "$marker_current" "$marker_headers")
		case "$marker_status" in
			200)
				current_version=$(read_marker_version "$marker_current")
				comparison=$("$release_support_tool" semver-compare "$current_version" "$tag")
				case "$comparison" in 0|1) printf '%s' "$current_version"; return 0 ;;
					-1) ;;
					*) fail "release support returned an invalid semantic-version comparison" ;;
				esac
				marker_etag=$(extract_etag "$marker_headers")
				case "$marker_etag" in \"*\") marker_condition="If-Match: $marker_etag" ;;
					*) fail "R2 returned an invalid latest VERSION ETag" ;;
				esac
				;;
			404) marker_condition='If-None-Match: *' ;;
			*) fail "latest VERSION precondition returned HTTP $marker_status" ;;
		esac
		marker_put=$(put_conditionally oberth/latest/VERSION "$release_dir/VERSION" 'text/plain; charset=utf-8' "$marker_condition")
		case "$marker_put" in 200|201) printf '%s' "$tag"; return 0 ;;
			409|412) sleep 1 ;;
			*) fail "latest VERSION write returned HTTP $marker_put" ;;
		esac
		marker_attempt=$((marker_attempt + 1))
	done
	fail "latest VERSION did not converge after concurrent releases"
)

stage_versioned_release() (
	stage_version=$tag
	stage_dir=$1
	rm -rf -- "$stage_dir"
	mkdir -p "$stage_dir"
	for stage_file in \
		oberth-linux-amd64 \
		oberth-linux-arm64 \
		oberth-darwin-amd64 \
		oberth-darwin-arm64 \
		SHA256SUMS \
		SHA256SUMS.sigstore.json \
		cosign.pub \
		release.json \
		release.json.sigstore.json; do
		stage_headers=$stage_dir/$stage_file.headers
		stage_status=$(authoritative_object_status "oberth/$stage_version/$stage_file" "$stage_dir/$stage_file" "$stage_headers")
		[ "$stage_status" = 200 ] || fail "versioned release $stage_version lacks $stage_file in authoritative R2"
		rm -f -- "$stage_headers"
		cmp -s "$release_dir/$stage_file" "$stage_dir/$stage_file" || fail "versioned release $stage_version differs from the runtime-verified candidate: $stage_file"
	done
	cmp -s "$release_dir/cosign.pub" "$stage_dir/cosign.pub" || fail "versioned release $stage_version uses an untrusted signing key"
	(cd "$stage_dir" && sha256sum -c SHA256SUMS >/dev/null) || fail "versioned release $stage_version has invalid checksums"
	"$cosign_tool" verify-blob --key "$release_dir/cosign.pub" --insecure-ignore-tlog --bundle "$stage_dir/SHA256SUMS.sigstore.json" "$stage_dir/SHA256SUMS" >/dev/null || \
		fail "versioned release $stage_version has an invalid signature bundle"
	"$cosign_tool" verify-blob --key "$release_dir/cosign.pub" --insecure-ignore-tlog --bundle "$stage_dir/release.json.sigstore.json" "$stage_dir/release.json" >/dev/null || \
		fail "versioned release $stage_version has an invalid release.json signature bundle"
	# Only this tag's receipt authorizes these bytes. Newer releases are
	# handled as supersession before staging, never by executing their data.
)

converge_latest_aliases() {
	converge_attempt=1
	while [ "$converge_attempt" -le 10 ]; do
		converge_marker=$release_dir/.converge-VERSION
		converge_headers=$release_dir/.converge-VERSION.headers
		converge_status=$(authoritative_object_status oberth/latest/VERSION "$converge_marker" "$converge_headers")
		[ "$converge_status" = 200 ] || fail "latest VERSION disappeared during finalization"
		converge_version=$(read_marker_version "$converge_marker")
		[ "$converge_version" = "$tag" ] || fail "latest VERSION changed while the finalizer mutex was held; refusing other release bytes"
		converge_dir=$release_dir/.latest-target
		for converge_file in \
			oberth-linux-amd64 \
			oberth-linux-arm64 \
			oberth-darwin-amd64 \
			oberth-darwin-arm64 \
			SHA256SUMS \
			SHA256SUMS.sigstore.json \
			cosign.pub \
			release.json \
			release.json.sigstore.json; do
			put_mutable_exact "oberth/latest/$converge_file" "$converge_dir/$converge_file" "$(artifact_content_type "$converge_file")"
		done
		converge_after=$release_dir/.converge-VERSION-after
		converge_after_headers=$release_dir/.converge-VERSION-after.headers
		converge_after_status=$(authoritative_object_status oberth/latest/VERSION "$converge_after" "$converge_after_headers")
		if [ "$converge_after_status" = 200 ] && cmp -s "$converge_marker" "$converge_after"; then
			wait_for_exact_public_object "$release_origin/oberth/latest/VERSION" "$converge_marker"
			for converge_file in \
				oberth-linux-amd64 \
				oberth-linux-arm64 \
				oberth-darwin-amd64 \
				oberth-darwin-arm64 \
				SHA256SUMS \
				SHA256SUMS.sigstore.json \
				cosign.pub \
				release.json \
				release.json.sigstore.json; do
				wait_for_exact_public_object "$release_origin/oberth/latest/$converge_file" "$converge_dir/$converge_file"
			done
			final_marker=$release_dir/.converge-VERSION-final
			final_headers=$release_dir/.converge-VERSION-final.headers
			final_status=$(authoritative_object_status oberth/latest/VERSION "$final_marker" "$final_headers")
			if [ "$final_status" = 200 ] && cmp -s "$converge_marker" "$final_marker"; then
				return 0
			fi
		fi
		converge_attempt=$((converge_attempt + 1))
	done
	fail "latest aliases did not converge to a stable VERSION marker"
}

finalize_chart_index() (
	chart_version=${tag#v}
	chart_name=oberth-${chart_version}.tgz
	chart_path=$release_dir/$chart_name
	chart_digest=$(sha256sum "$chart_path" | cut -d ' ' -f 1)
	chart_url=$chart_repo/$chart_name
	index_attempt=1
	while [ "$index_attempt" -le 10 ]; do
		current=$release_dir/.chart-index-current
		headers=$release_dir/.chart-index-current.headers
		status=$(authoritative_object_status oberth/index.yaml "$current" "$headers")
		merge_argument=
		case "$status" in
			200)
				state=$("$release_support_tool" chart-index-state "$current" "$chart_version" "$chart_digest" "$chart_url")
				if [ "$state" = exact ]; then
					wait_for_exact_public_object "$chart_repo/index.yaml" "$current"
					return 0
				fi
				etag=$(extract_etag "$headers")
				case "$etag" in \"*\") condition="If-Match: $etag" ;;
					*) fail "R2 returned an invalid chart index ETag" ;;
				esac
				merge_argument=$current
				;;
			404) condition='If-None-Match: *' ;;
			*) fail "chart index precondition returned HTTP $status" ;;
		esac
		stage=$release_dir/.chart-index-stage
		rm -rf -- "$stage"
		mkdir -p "$stage"
		cp "$chart_path" "$stage/$chart_name"
		if [ -n "$merge_argument" ]; then
			"$helm_tool" repo index "$stage" --url "$chart_repo" --merge "$merge_argument"
		else
			"$helm_tool" repo index "$stage" --url "$chart_repo"
		fi
		# Helm preserves historical archive URLs when merging. Move every
		# version to the canonical host without changing its chart or digest.
		"$release_support_tool" chart-index-rebase "$stage/index.yaml" "$chart_repo" "$stage/rebased.yaml"
		mv "$stage/rebased.yaml" "$stage/index.yaml"
		"$release_support_tool" chart-index-state "$stage/index.yaml" "$chart_version" "$chart_digest" "$chart_url" | grep -Fx exact >/dev/null || \
			fail "generated chart index does not bind the exact release chart"
		put_status=$(put_conditionally oberth/index.yaml "$stage/index.yaml" 'text/yaml; charset=utf-8' "$condition")
		case "$put_status" in
			200|201)
				readback=$release_dir/.chart-index-readback
				readback_headers=$release_dir/.chart-index-readback.headers
				readback_status=$(authoritative_object_status oberth/index.yaml "$readback" "$readback_headers")
				[ "$readback_status" = 200 ] || fail "chart index readback returned HTTP $readback_status"
				"$release_support_tool" chart-index-state "$readback" "$chart_version" "$chart_digest" "$chart_url" | grep -Fx exact >/dev/null || \
					fail "authoritative chart index lost the exact release entry"
				wait_for_exact_public_object "$chart_repo/index.yaml" "$readback"
				return 0
				;;
			409|412) sleep 1 ;;
			*) fail "chart index write returned HTTP $put_status" ;;
		esac
		index_attempt=$((index_attempt + 1))
	done
	fail "chart index did not converge after concurrent releases"
)

finalize_release() {
	# The verifier ran downloaded code without credentials and only its
	# dedicated receipt is accepted here. This action reads public signing
	# material, performs static checks, and never executes release artifacts.
	validate_source
	require_artifacts
	test -s "$release_dir/cosign.pub" || fail "release-verify or release-publish must run before finalize (cosign.pub missing from shared work volume)"
	require_runtime_receipt
	initialize_authoritative_r2
	# Bind authoritative bytes to the accepted candidate before any marker
	# or alias write. The staged files remain private to this finalizer.
	stage_versioned_release "$release_dir/.latest-target"
	# The template's per-repository finalizer mutex serializes marker and
	# alias updates. The receipt binds only this tag, not a newer winner.
	selected_version=$(advance_latest_marker)
	if [ "$selected_version" != "$tag" ]; then
		finalize_chart_index
		printf 'Oberth release %s is superseded by %s; latest binaries unchanged\n' "$tag" "$selected_version"
		return
	fi
	converge_latest_aliases
	# -------------------------------------------------------------------
	# Prune orphan objects from oberth/latest/ that are not in the converged
	# set. converged = {VERSION, SHA256SUMS, SHA256SUMS.sigstore.json,
	# cosign.pub} ∪ {artifact names in SHA256SUMS}.
	# Fail closed on list failure; refuse keys outside oberth/latest/.
	# Runs after convergence is confirmed. (#703/#719)
	# -------------------------------------------------------------------
	list_status=$(curl --silent --show-error --proto '=https' --tlsv1.2 --max-redirs 0 \
		--connect-timeout 10 --max-time 60 --max-filesize 1048576 \
		--config "$r2_curl_config" \
		--output "$release_dir/.listing.xml" --write-out '%{http_code}' \
		"$r2_endpoint/$r2_bucket?list-type=2&prefix=oberth/latest/") || fail 'latest/ listing transport failed'
	[ "$list_status" = 200 ] || fail "latest/ listing returned status $list_status"
	/usr/bin/python3 -I -S -B -c '
import sys, xml.etree.ElementTree as ET
sumsf, listf, pfx = sys.argv[1], sys.argv[2], sys.argv[3]
fixed = {"VERSION", "SHA256SUMS", "SHA256SUMS.sigstore.json", "cosign.pub", "release.json", "release.json.sigstore.json"}
artifacts = set()
for line in open(sumsf):
    parts = line.strip().split("  ", 1)
    if len(parts) == 2:
        artifacts.add(parts[1])
expected = fixed | artifacts
tree = ET.parse(listf)
root = tree.getroot()
ns = root.tag.split("}")[0] + "}" if "}" in root.tag else ""
truncated = root.find(ns + "IsTruncated")
if truncated is not None and truncated.text == "true":
    print("listing truncated", file=sys.stderr)
    sys.exit(1)
keys = [elem.text for elem in root.iter(ns + "Key") if elem.text]
for key in keys:
    if not key.startswith(pfx):
        print("key outside prefix: " + key, file=sys.stderr)
        sys.exit(2)
    name = key[len(pfx):]
    if name and name not in expected:
        print(name)
' "$release_dir/SHA256SUMS" "$release_dir/.listing.xml" "oberth/latest/" > "$release_dir/.orphans" || {
		rc=$?
		[ "$rc" -eq 2 ] && fail 'listing returned key outside oberth/latest/ prefix'
		fail 'latest/ listing parse failed'
	}
	while IFS= read -r orphan; do
		[ -n "$orphan" ] || continue
		del_status=$(curl --silent --show-error --proto '=https' --tlsv1.2 --max-redirs 0 \
			--connect-timeout 10 --max-time 60 \
			--config "$r2_curl_config" \
			--output "$release_dir/.r2-delete-response" --write-out '%{http_code}' --request DELETE \
			"$r2_endpoint/$r2_bucket/oberth/latest/$orphan") || fail "orphan delete transport failed: oberth/latest/$orphan"
		case "$del_status" in
			200|204) printf 'release: pruned orphan oberth/latest/%s\n' "$orphan" ;;
			*) fail "orphan delete failed: oberth/latest/$orphan (status $del_status)" ;;
		esac
	done < "$release_dir/.orphans"
	finalize_chart_index
	final_marker=$release_dir/.stable-VERSION
	final_headers=$release_dir/.stable-VERSION.headers
	final_status=$(authoritative_object_status oberth/latest/VERSION "$final_marker" "$final_headers")
	[ "$final_status" = 200 ] || fail "stable VERSION disappeared after finalization"
	stable_version=$(read_marker_version "$final_marker")
	[ "$stable_version" = "$tag" ] || fail "latest VERSION changed while the finalizer mutex was held"
	printf 'Stable Oberth release is %s\n' "$stable_version"
}

# Print the reviewed digest from a one-line `sha256sum -c` pin, requiring the
# exact production path so a pin cannot be retargeted at another file.
pinned_digest() {
	awk -v want="$2" '
		NR == 1 && NF == 2 && length($1) == 64 && $1 ~ /^[0-9a-f]+$/ && $2 == want { digest = $1 }
		END { if (NR != 1 || digest == "") exit 1; print digest }
	' "$1"
}

require_pinned_file() {
	file=$1
	pin=$2
	pin_path=$3
	if [ ! -f "$file" ] || [ -L "$file" ]; then
		fail "$file is missing or not a regular file"
	fi
	want_digest=$(pinned_digest "$pin" "$pin_path") || fail "$pin is malformed"
	[ "$(sha256sum "$file" | cut -d ' ' -f 1)" = "$want_digest" ] || fail "$file differs from the reviewed pin $pin"
}

# The one wrangler version website/package.json declares, the lockfile
# resolves, and the lockfile root records. Anything else fails closed.
lockfile_wrangler_version() {
	/usr/bin/python3 -I -S -B - website/package.json website/package-lock.json <<'PY'
import json
import re
import sys

with open(sys.argv[1], 'rb') as stream:
    manifest = json.load(stream)
with open(sys.argv[2], 'rb') as stream:
    lock = json.load(stream)
declared = manifest['devDependencies']['wrangler']
if (not isinstance(declared, str) or not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', declared)
        or lock['packages']['']['devDependencies']['wrangler'] != declared
        or lock['packages']['node_modules/wrangler']['version'] != declared):
    sys.exit(1)
print(declared)
PY
}

# Extract the pinned Node runtime into Pod-private scratch and prove the
# resulting executable is the reviewed one.
unpack_pinned_node() {
	require_pinned_file "$1" "$website_node_pin" /tmp/oberth-website-inputs/node.tar.gz
	rm -rf -- "$website_dir/node"
	mkdir -p "$website_dir/node"
	tar -xzf "$1" -C "$website_dir/node" --no-same-owner --strip-components=1
	require_pinned_file "$website_node" "$website_node_bin_pin" /tmp/oberth-website/node/bin/node
}

# npm with a fixed environment: no user or global configuration, the public
# registry named explicitly, lifecycle scripts disabled, integrity from the
# committed lockfile only.
run_website_npm() (
	cd "$website_app" || exit 1
	# npm refuses one file for both levels; two empty Pod-private files.
	: >"$website_dir/npmrc-user"
	: >"$website_dir/npmrc-global"
	HOME=$website_dir/home
	npm_config_userconfig=$website_dir/npmrc-user
	npm_config_globalconfig=$website_dir/npmrc-global
	npm_config_update_notifier=false
	export HOME npm_config_userconfig npm_config_globalconfig npm_config_update_notifier
	exec "$website_node" "$website_npm" ci --ignore-scripts --no-audit --no-fund \
		--registry=https://registry.npmjs.org/ --logs-dir="$website_dir/npm-logs" "$@"
)

website_wrangler_version() (
	cd "$website_app" || exit 1
	HOME=$website_dir/home
	XDG_CONFIG_HOME=$website_dir/home/.config
	WRANGLER_SEND_METRICS=false
	WRANGLER_HIDE_BANNER=true
	export HOME XDG_CONFIG_HOME WRANGLER_SEND_METRICS WRANGLER_HIDE_BANNER
	"$website_node" "$website_wrangler" --version | tail -n 1 | tr -d '[:space:]'
)

# The release publishes exactly the reviewed Worker: this name, these two
# custom domains, no workers.dev or preview URLs, static assets only (no
# script, no bindings). A configuration change must move this check too.
verify_website_config() {
	/usr/bin/python3 -I -S -B - website/wrangler.jsonc <<'PY' || fail "website/wrangler.jsonc is not the reviewed oberth.ci Worker configuration"
import json
import sys


def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError()
        result[key] = value
    return result


try:
    with open(sys.argv[1], 'rb') as stream:
        config = json.load(stream, object_pairs_hook=unique)
except ValueError:
    sys.exit(1)
assets = config.get('assets') if isinstance(config, dict) else None
if (not isinstance(config, dict) or not isinstance(assets, dict)
        or not set(config) <= {'$schema', 'name', 'compatibility_date', 'workers_dev', 'preview_urls', 'assets', 'routes'}
        or config.get('name') != 'oberth-ci'
        or config.get('workers_dev') is not False or config.get('preview_urls') is not False
        or config.get('routes') != [{'pattern': 'oberth.ci', 'custom_domain': True},
                                    {'pattern': 'www.oberth.ci', 'custom_domain': True}]
        or not set(assets) <= {'directory', 'not_found_handling', 'run_worker_first', 'html_handling'}
        or assets.get('directory') != './public' or assets.get('run_worker_first') is not False):
    sys.exit(1)
PY
}

# Website staging and installation run before any secret exists in their Pod.
require_credential_free_step() {
	[ ! -e /var/run/secrets/kubernetes.io/serviceaccount/token ] || fail "website staging must run in a tokenless Pod"
	[ -z "${OBERTH_SECRETSTORE_DIR:-}${COSIGN_KEY:-}${COSIGN_PASSWORD:-}${VAULT_TOKEN:-}${GOOGLE_APPLICATION_CREDENTIALS:-}${CLOUDFLARE_API_TOKEN:-}" ] || \
		fail "website staging refuses credential environment"
}

# Credential-free setup step: stage the inputs the release-website leaf
# re-verifies. Only npm from the pinned Node runs here, with lifecycle scripts
# disabled; no package code executes. The claim keeps the tarball (from
# fetch-node) and the content-addressed npm cache, nothing executable.
stage_website_packages() {
	validate_source
	require_credential_free_step
	[ -d "$website_inputs" ] || fail "the website inputs claim is not mounted at $website_inputs"
	wrangler_version=$(lockfile_wrangler_version) || fail "website/package.json and package-lock.json must pin one exact wrangler version"
	rm -rf -- "$website_dir"
	mkdir -p "$website_dir/home" "$website_app"
	cp "$website_inputs/node.tar.gz" "$website_dir/node.tar.gz"
	unpack_pinned_node "$website_dir/node.tar.gz"
	cp website/package.json website/package-lock.json "$website_app/"
	rm -rf -- "$website_inputs/npm-cache"
	run_website_npm --cache="$website_inputs/npm-cache"
	[ -f "$website_wrangler" ] || fail "npm did not install wrangler $wrangler_version"
	printf 'staged the npm cache for wrangler %s from the committed lockfile\n' "$wrangler_version"
}

# Final init container of release-website (no secret exists in the Pod yet):
# verify-website-inputs copied the hash-checked tarball and the npm cache into
# Pod-private scratch; install from them offline so every package is checked
# against the lockfile's sha512 integrity.
install_website_tools() {
	validate_source
	require_credential_free_step
	wrangler_version=$(lockfile_wrangler_version) || fail "website/package.json and package-lock.json must pin one exact wrangler version"
	if [ ! -f "$website_dir/node.tar.gz" ] || [ ! -d "$website_dir/npm-cache" ]; then
		fail "verified website inputs are missing; verify-website-inputs must run first"
	fi
	unpack_pinned_node "$website_dir/node.tar.gz"
	rm -rf -- "$website_app"
	mkdir -p "$website_dir/home" "$website_app"
	cp website/package.json website/package-lock.json "$website_app/"
	run_website_npm --offline --cache="$website_dir/npm-cache"
	installed_version=$(website_wrangler_version) || fail "installed wrangler does not run"
	[ "$installed_version" = "$wrangler_version" ] || fail "installed wrangler reports $installed_version, the lockfile pins $wrangler_version"
	printf 'installed wrangler %s offline from the lockfile-verified cache\n' "$installed_version"
}

# Credentialed: deploy the tagged website with the one scoped token. The token
# reaches wrangler only through its documented environment interface (a shell
# assignment from the memory-backed secret file, never argv); wrangler's home,
# config and log files live on the same memory-backed mount.
publish_website() {
	validate_source
	require_pinned_file "$website_node" "$website_node_bin_pin" /tmp/oberth-website/node/bin/node
	if [ ! -f "$website_wrangler" ] || [ -L "$website_wrangler" ]; then
		fail "wrangler is not installed; install-website-tools must run first"
	fi
	verify_website_config
	rm -rf -- "$website_site"
	mkdir -p "$website_site"
	cp website/wrangler.jsonc "$website_site/wrangler.jsonc"
	cp -R website/public "$website_site/public"
	token_file=$secret_root/cloudflare-oberth-workers-token/CLOUDFLARE_API_TOKEN
	test -s "$token_file" || fail "cloudflare-oberth-workers-token/CLOUDFLARE_API_TOKEN is missing"
	ensure_cred_dir
	wrangler_home=$cred_dir/.wrangler-home
	rm -rf -- "$wrangler_home"
	mkdir -m 0700 "$wrangler_home"
	website_home_owned=true
	if ! (
		cd "$website_site" || exit 1
		unset OBERTH_SECRETSTORE_DIR OBERTH_SECRET_ROOT OBERTH_CREDENTIAL_DIR
		HOME=$wrangler_home
		XDG_CONFIG_HOME=$wrangler_home/.config
		WRANGLER_LOG_PATH=$wrangler_home/logs
		WRANGLER_LOG_SANITIZE=true
		WRANGLER_SEND_METRICS=false
		WRANGLER_HIDE_BANNER=true
		CLOUDFLARE_ACCOUNT_ID=$website_account_id
		export HOME XDG_CONFIG_HOME WRANGLER_LOG_PATH WRANGLER_LOG_SANITIZE WRANGLER_SEND_METRICS WRANGLER_HIDE_BANNER CLOUDFLARE_ACCOUNT_ID
		CLOUDFLARE_API_TOKEN=$(tr -d '[:space:]' <"$token_file")
		[ -n "$CLOUDFLARE_API_TOKEN" ] || exit 1
		export CLOUDFLARE_API_TOKEN
		exec "$website_node" "$website_wrangler" deploy \
			--config "$website_site/wrangler.jsonc" --autoconfig=false \
			--tag "$tag" --message "oberth $tag ($sha)"
	); then
		fail "wrangler deploy of the oberth.ci Worker failed"
	fi
	printf 'deployed the oberth.ci Worker (oberth-ci) from %s (%s)\n' "$tag" "$sha"
}

case "$action" in
	validate) validate_source ;;
	build) build_artifacts ;;
	sign-binaries) sign_binaries ;;
	publish-images) publish_images ;;
	publish-homebrew) publish_homebrew_tap ;;
	package-chart) prepare_chart ;;
	publish-chart) publish_chart ;;
	verify) verify_release ;;
	verify-public-runtime) verify_public_runtime ;;
	finalize) finalize_release ;;
	publish-public) publish_public_release ;;
	cloudflare-phase) cloudflare_phase "${2:-}" ;;
	stage-website-packages) stage_website_packages ;;
	install-website-tools) install_website_tools ;;
	publish-website) publish_website ;;
	*) fail "usage: $0 validate|build|sign-binaries|publish-images|publish-homebrew|package-chart|publish-chart|publish-public|cloudflare-phase|verify|verify-public-runtime|finalize|stage-website-packages|install-website-tools|publish-website" ;;
esac
