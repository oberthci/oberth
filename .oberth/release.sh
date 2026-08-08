#!/bin/sh
set -eu

# The shared runner is intentionally only a bootstrap substrate. All build,
# scan, signing, registry, and chart behavior for an Oberth release lives here.
umask 077

action=${1:-}
tag=${OBERTH_RELEASE_TAG:-}
sha=${OBERTH_RELEASE_SHA:-}
release_dir=${OBERTH_RELEASE_DIR:-/tmp/oberth-release}
secret_root=${OBERTH_SECRET_ROOT:-/secrets}
release_origin=${OBERTH_RELEASE_ORIGIN:-https://releases.cloudtaser.io}
chart_repo=${OBERTH_CHART_REPOSITORY:-https://charts.cloudtaser.io/oberth}
r2_account=${OBERTH_R2_ACCOUNT:-7f6f91396004ae60da4e272c1193ec6e}
r2_bucket=${OBERTH_R2_BUCKET:-cloudtaser-releases}
r2_endpoint=${OBERTH_R2_ENDPOINT:-https://${r2_account}.r2.cloudflarestorage.com}
r2_curl_config=${OBERTH_R2_CURL_CONFIG:-$release_dir/.r2-curl.conf}
gar_host=europe-west4-docker.pkg.dev
gar_chart_oci=oci://${gar_host}/skipopsmain/cloudtaser-helm
gar_chart=${gar_host}/skipopsmain/cloudtaser-helm/oberth
gar_key=$secret_root/gar-sa-key/key.json
r2_config_owned=false
registry_config_owned=false

fail() {
	printf 'release: %s\n' "$*" >&2
	exit 1
}

case "$release_dir" in
	/tmp/oberth-release|/tmp/oberth-release-*) ;;
	*) fail "OBERTH_RELEASE_DIR must be a dedicated /tmp/oberth-release path" ;;
esac
[ ! -L "$release_dir" ] || fail "release directory must not be a symlink"

cleanup() {
	if [ "$r2_config_owned" = true ]; then
		rm -f -- "$r2_curl_config"
	fi
	if [ "$registry_config_owned" = true ]; then
		rm -rf -- "$release_dir/.docker" "$release_dir/.helm"
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
		oberth-runner-linux-amd64 \
		oberth-runner-linux-arm64 \
		SHA256SUMS; do
		test -s "$release_dir/$file" || fail "missing release artifact $file"
	done
	[ "$(wc -l <"$release_dir/SHA256SUMS" | tr -d ' ')" = 6 ] || fail "SHA256SUMS must bind exactly six executables"
	(cd "$release_dir" && sha256sum -c SHA256SUMS >/dev/null) || fail "local release artifact checksum mismatch"
}

native_binary() {
	case "$(uname -m)" in
		x86_64|amd64) printf '%s' oberth-linux-amd64 ;;
		aarch64|arm64) printf '%s' oberth-linux-arm64 ;;
		*) fail "unsupported release-runner architecture $(uname -m)" ;;
	esac
}

verify_binary_version() {
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
	command -v go >/dev/null 2>&1 || fail "Go is not installed"
	if [ -e "$release_dir" ]; then
		rm -rf -- "$release_dir"
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
		GOOS=$goos GOARCH=$goarch go build \
			-trimpath -buildvcs=false \
			-ldflags="-s -w -buildid= -X main.version=${tag} -X main.commit=${short_sha} -X main.date=${created}" \
			-o "$release_dir/$output" ./cmd/oberth
		GOOS=$goos GOARCH=$goarch go build \
			-trimpath -buildvcs=false \
			-ldflags="-s -w -buildid= -X main.version=${tag} -X main.commit=${short_sha} -X main.date=${created}" \
			-o "$release_dir/$output.repro" ./cmd/oberth
		cmp -s "$release_dir/$output" "$release_dir/$output.repro" || fail "$output is not reproducible"
		rm -f -- "$release_dir/$output.repro"
		go version -m "$release_dir/$output" | grep -Eq '^[[:space:]]*path[[:space:]]+github\.com/oberthci/oberth/cmd/oberth$' || \
			fail "$output does not identify the Oberth command module"
	done

	for goarch in amd64 arm64; do
		output=oberth-runner-linux-${goarch}
		GOOS=linux GOARCH=$goarch go build \
			-trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
			-o "$release_dir/$output" ./cmd/oberth-runner
		GOOS=linux GOARCH=$goarch go build \
			-trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
			-o "$release_dir/$output.repro" ./cmd/oberth-runner
		cmp -s "$release_dir/$output" "$release_dir/$output.repro" || fail "$output is not reproducible"
		rm -f -- "$release_dir/$output.repro"
	done

	(
		cd "$release_dir"
		sha256sum \
			oberth-linux-amd64 \
			oberth-linux-arm64 \
			oberth-darwin-amd64 \
			oberth-darwin-arm64 \
			oberth-runner-linux-amd64 \
			oberth-runner-linux-arm64 >SHA256SUMS
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
	token_file=$secret_root/r2-upload-token/token
	test -s "$token_file" || fail "r2-upload-token/token is missing"
	command -v oberth-release-support >/dev/null 2>&1 || fail "oberth-release-support is not installed"
	oberth-release-support r2-auth "$token_file" "$r2_account" "$r2_bucket" oberth/ "$r2_curl_config"
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
	key_file=$secret_root/cosign-secret/cosign.key
	password_file=$secret_root/cosign-secret/cosign.password
	expected_public_key=$secret_root/cosign-secret/cosign.pub
	test -s "$key_file" || fail "cosign-secret/cosign.key is missing"
	command -v cosign >/dev/null 2>&1 || fail "cosign is not installed"
	COSIGN_KEY=$(cat "$key_file")
	export COSIGN_KEY
	COSIGN_PASSWORD=
	if [ -f "$password_file" ]; then
		COSIGN_PASSWORD=$(cat "$password_file")
	fi
	export COSIGN_PASSWORD COSIGN_YES=true
	cosign public-key --key env://COSIGN_KEY >"$release_dir/cosign.pub"
	test -s "$release_dir/cosign.pub" || fail "cosign produced an empty public key"
	if [ -f "$expected_public_key" ]; then
		cmp -s "$expected_public_key" "$release_dir/cosign.pub" || fail "mounted cosign public key does not match its private key"
	fi
}

clear_signing_identity() {
	unset COSIGN_KEY COSIGN_PASSWORD
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
			cosign verify-blob --key "$release_dir/cosign.pub" --bundle "$bundle" "$payload" >/dev/null || \
				fail "existing immutable signature bundle does not authenticate its payload"
			;;
		404)
			rm -f -- "$existing" "$existing_headers"
			cosign sign-blob --key env://COSIGN_KEY --bundle "$bundle" "$payload" >/dev/null
			test -s "$bundle" || fail "cosign produced an empty signature bundle"
			cosign verify-blob --key "$release_dir/cosign.pub" --bundle "$bundle" "$payload" >/dev/null || \
				fail "new signature bundle does not authenticate its payload"
			put_status=$(put_conditionally "$object_key" "$bundle" application/json 'If-None-Match: *')
			case "$put_status" in 200|201|409|412) ;;
				*) fail "immutable signature create returned HTTP $put_status" ;;
			esac
			readback=$release_dir/.authoritative-bundle
			readback_headers=$release_dir/.authoritative-bundle.headers
			readback_status=$(authoritative_object_status "$object_key" "$readback" "$readback_headers")
			[ "$readback_status" = 200 ] || fail "immutable signature readback returned HTTP $readback_status"
			cosign verify-blob --key "$release_dir/cosign.pub" --bundle "$readback" "$payload" >/dev/null || \
				fail "authoritative signature bundle does not authenticate its payload after HTTP $put_status"
			mv "$readback" "$bundle"
			rm -f -- "$readback_headers"
			;;
		*) fail "immutable signature precondition returned HTTP $bundle_status" ;;
	esac
	rm -f -- "$existing_headers"
	wait_for_exact_public_object "$public_url" "$bundle"
}

publish_r2_binaries() {
	validate_source
	require_artifacts
	initialize_authoritative_r2
	prepare_signing_identity
	for file in \
		oberth-linux-amd64 \
		oberth-linux-arm64 \
		oberth-darwin-amd64 \
		oberth-darwin-arm64 \
		oberth-runner-linux-amd64 \
		oberth-runner-linux-arm64; do
		publish_exact_object "oberth/$tag/$file" "$release_dir/$file" application/octet-stream "$release_origin/oberth/$tag/$file"
	done
	publish_exact_object "oberth/$tag/SHA256SUMS" "$release_dir/SHA256SUMS" 'text/plain; charset=utf-8' "$release_origin/oberth/$tag/SHA256SUMS"
	publish_exact_object "oberth/$tag/cosign.pub" "$release_dir/cosign.pub" 'text/plain; charset=utf-8' "$release_origin/oberth/$tag/cosign.pub"
	publish_signed_bundle \
		"oberth/$tag/SHA256SUMS.sigstore.json" \
		"$release_origin/oberth/$tag/SHA256SUMS.sigstore.json" \
		"$release_dir/SHA256SUMS" \
		"$release_dir/SHA256SUMS.sigstore.json"
	clear_signing_identity
}

prepare_registry_auth() {
	test -s "$gar_key" || fail "gar-sa-key/key.json is missing"
	rm -rf -- "$release_dir/.docker" "$release_dir/.helm"
	mkdir -p "$release_dir/.docker" "$release_dir/.helm/registry"
	auth=$( (printf '%s' '_json_key:'; cat "$gar_key") | base64 | tr -d '\n')
	printf '{"auths":{"%s":{"auth":"%s"}}}\n' "$gar_host" "$auth" >"$release_dir/.docker/config.json"
	chmod 0600 "$release_dir/.docker/config.json"
	DOCKER_CONFIG=$release_dir/.docker
	HELM_REGISTRY_CONFIG=$release_dir/.helm/registry/config.json
	export DOCKER_CONFIG HELM_REGISTRY_CONFIG
	registry_config_owned=true
}

sign_registry_object() {
	reference=$1
	if ! cosign verify --key "$release_dir/cosign.pub" "$reference" >/dev/null 2>&1; then
		cosign sign --key env://COSIGN_KEY "$reference" >/dev/null
	fi
	cosign verify --key "$release_dir/cosign.pub" "$reference" >/dev/null || fail "registry signature verification failed for $reference"
}

scan_image() {
	reference=$1
	trivy image \
		--cache-dir /tmp/oberth-trivy \
		--skip-check-update --skip-vex-repo-update --skip-version-check \
		--disable-telemetry --scanners vuln \
		--exit-code 1 --severity HIGH,CRITICAL "$reference"
}

publish_images() {
	validate_source
	require_artifacts
	command -v oberth-release-image >/dev/null 2>&1 || fail "oberth-release-image is not installed"
	command -v trivy >/dev/null 2>&1 || fail "Trivy is not installed"
	prepare_registry_auth
	created=$(source_created)
	oberth-release-image publish "$tag" "$sha" "$created" "$release_dir" "$gar_key"
	for reference_file in server-image.txt runner-image.txt; do
		reference=$(sed -n '1p' "$release_dir/$reference_file")
		[ -n "$reference" ] || fail "release image helper produced an empty reference"
		scan_image "$reference"
	done
	prepare_signing_identity
	for reference_file in server-image.txt runner-image.txt; do
		reference=$(sed -n '1p' "$release_dir/$reference_file")
		sign_registry_object "$reference"
	done
	clear_signing_identity
	oberth-release-image verify "$tag" "$sha" "$created" "$release_dir" "$gar_key"
}

prepare_chart() {
	validate_source
	require_artifacts
	command -v helm >/dev/null 2>&1 || fail "Helm is not installed"
	command -v oberth-release-support >/dev/null 2>&1 || fail "oberth-release-support is not installed"
	server_ref=$(sed -n '1p' "$release_dir/server-image.txt")
	runner_ref=$(sed -n '1p' "$release_dir/runner-image.txt")
	case "$server_ref" in ${gar_host}/skipopsmain/cloudtaser/cloudtaser-oberth@sha256:*) ;;
		*) fail "server image reference is not an immutable Oberth GAR digest" ;;
	esac
	case "$runner_ref" in ${gar_host}/skipopsmain/cloudtaser/cloudtaser-oberth-ci@sha256:*) ;;
		*) fail "runner image reference is not an immutable Oberth GAR digest" ;;
	esac
	chart_source=$release_dir/chart-source
	rm -rf -- "$chart_source" "$release_dir/chart-a" "$release_dir/chart-b"
	cp -R charts/oberth "$chart_source"
	if ! awk -v server="$server_ref" -v runner="$runner_ref" '
		/^image:$/ { section = "server"; print; next }
		/^runnerImage:$/ { section = "runner"; print; next }
		/^[^[:space:]]/ { section = "" }
		section == "server" && $1 == "ref:" { print "  ref: " server; servers++; next }
		section == "runner" && $1 == "ref:" { print "  ref: " runner; runners++; next }
		{ print }
		END { if (servers != 1 || runners != 1) exit 42 }
	' "$chart_source/values.yaml" >"$chart_source/values.yaml.new"; then
		fail "chart values do not contain exactly one server and runner image reference"
	fi
	mv "$chart_source/values.yaml.new" "$chart_source/values.yaml"
	created=$(source_created)
	oberth-release-support normalize-tree "$chart_source" "$created"
	helm lint "$chart_source"
	helm template oberth "$chart_source" --namespace oberth >"$release_dir/chart-rendered.yaml"
	grep -Fq "$server_ref" "$release_dir/chart-rendered.yaml" || fail "release chart does not render the exact server digest"
	grep -Fq "$runner_ref" "$release_dir/chart-rendered.yaml" || fail "release chart does not render the exact runner digest"
	chart_version=${tag#v}
	mkdir -p "$release_dir/chart-a" "$release_dir/chart-b"
	helm package "$chart_source" --version "$chart_version" --app-version "$tag" --destination "$release_dir/chart-a" >/dev/null
	helm package "$chart_source" --version "$chart_version" --app-version "$tag" --destination "$release_dir/chart-b" >/dev/null
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
	helm pull "$gar_chart_oci/oberth" --version "${tag#v}" --destination "$destination" >/dev/null
}

publish_chart() {
	validate_source
	require_artifacts
	require_chart
	initialize_authoritative_r2
	prepare_registry_auth
	helm registry login "$gar_host" --username _json_key --password-stdin <"$gar_key" >/dev/null
	chart_version=${tag#v}
	chart_tag=$(printf '%s' "$chart_version" | tr '+' '_')
	chart_name=oberth-${chart_version}.tgz
	chart_path=$release_dir/$chart_name
	tag_reference=$gar_chart:$chart_tag
	registry_state=$(oberth-release-support registry-digest "$tag_reference" "$gar_key")
	if [ "$registry_state" = missing ]; then
		if helm push "$chart_path" "$gar_chart_oci" >/dev/null; then
			:
		else
			push_status=$?
			registry_state=$(oberth-release-support registry-digest "$tag_reference" "$gar_key")
			[ "$registry_state" != missing ] || fail "GAR chart push failed with status $push_status and no exact object appeared"
		fi
	fi
	pull_gar_chart "$release_dir/gar-chart"
	cmp -s "$chart_path" "$release_dir/gar-chart/$chart_name" || fail "GAR chart version already exists with different bytes"
	chart_digest=$(oberth-release-support registry-digest "$tag_reference" "$gar_key")
	printf '%s\n' "$chart_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || fail "GAR did not return an immutable chart digest"
	chart_reference=$gar_chart@$chart_digest
	printf '%s\n' "$chart_reference" >"$release_dir/chart-image.txt"
	prepare_signing_identity
	sign_registry_object "$chart_reference"
	publish_exact_object "oberth/$chart_name" "$chart_path" application/gzip "$chart_repo/$chart_name"
	publish_exact_object "oberth/cosign.pub" "$release_dir/cosign.pub" 'text/plain; charset=utf-8' "$chart_repo/cosign.pub"
	publish_signed_bundle \
		"oberth/$chart_name.sigstore.json" \
		"$chart_repo/$chart_name.sigstore.json" \
		"$chart_path" \
		"$release_dir/$chart_name.sigstore.json"
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
		oberth-runner-linux-amd64 \
		oberth-runner-linux-arm64 \
		SHA256SUMS \
		SHA256SUMS.sigstore.json \
		cosign.pub; do
		download_public_object "$release_origin/oberth/$tag/$file" "$verify_dir/$file"
		cmp -s "$release_dir/$file" "$verify_dir/$file" || fail "downloaded $file differs from the published candidate"
	done
	(cd "$verify_dir" && sha256sum -c SHA256SUMS >/dev/null) || fail "downloaded artifact checksum mismatch"
	cosign verify-blob --key "$verify_dir/cosign.pub" --bundle "$verify_dir/SHA256SUMS.sigstore.json" "$verify_dir/SHA256SUMS" >/dev/null || \
		fail "downloaded binary signature bundle is invalid"
	verify_binary_version "$verify_dir/$(native_binary)" "$tag" "$sha"
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
	resolved=$(oberth-release-support registry-digest "$gar_chart:$chart_tag" "$gar_key")
	[ "$gar_chart@$resolved" = "$chart_reference" ] || fail "GAR chart tag does not resolve to the signed digest"
	cosign verify --key "$release_dir/cosign.pub" "$chart_reference" >/dev/null || fail "GAR chart signature is invalid"
	public_dir=$release_dir/verified-public-chart
	rm -rf -- "$public_dir"
	mkdir -p "$public_dir"
	download_public_object "$chart_repo/$chart_name" "$public_dir/$chart_name"
	download_public_object "$chart_repo/$chart_name.sigstore.json" "$public_dir/$chart_name.sigstore.json"
	download_public_object "$chart_repo/cosign.pub" "$public_dir/cosign.pub"
	cmp -s "$chart_path" "$public_dir/$chart_name" || fail "public chart differs from the source package"
	cmp -s "$release_dir/cosign.pub" "$public_dir/cosign.pub" || fail "public chart signing key differs from the mounted release key"
	cosign verify-blob --key "$public_dir/cosign.pub" --bundle "$public_dir/$chart_name.sigstore.json" "$public_dir/$chart_name" >/dev/null || \
		fail "public chart signature bundle is invalid"
	helm template oberth "$public_dir/$chart_name" --namespace oberth >"$public_dir/rendered.yaml"
	grep -Fq "$(sed -n '1p' "$release_dir/server-image.txt")" "$public_dir/rendered.yaml" || fail "public chart does not render the signed server image"
	grep -Fq "$(sed -n '1p' "$release_dir/runner-image.txt")" "$public_dir/rendered.yaml" || fail "public chart does not render the signed runner image"
}

verify_release() {
	validate_source
	require_artifacts
	command -v cosign >/dev/null 2>&1 || fail "cosign is not installed"
	command -v trivy >/dev/null 2>&1 || fail "Trivy is not installed"
	command -v helm >/dev/null 2>&1 || fail "Helm is not installed"
	command -v oberth-release-image >/dev/null 2>&1 || fail "oberth-release-image is not installed"
	prepare_signing_identity
	clear_signing_identity
	verify_public_binaries
	prepare_registry_auth
	created=$(source_created)
	oberth-release-image verify "$tag" "$sha" "$created" "$release_dir" "$gar_key"
	for reference_file in server-image.txt runner-image.txt; do
		reference=$(sed -n '1p' "$release_dir/$reference_file")
		cosign verify --key "$release_dir/cosign.pub" "$reference" >/dev/null || fail "GAR image signature is invalid"
		scan_image "$reference"
	done
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
	oberth-release-support semver-compare "$marker_version" "$marker_version" >/dev/null || fail "latest VERSION is not semantic"
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
				comparison=$(oberth-release-support semver-compare "$current_version" "$tag")
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
	stage_version=$1
	stage_dir=$2
	rm -rf -- "$stage_dir"
	mkdir -p "$stage_dir"
	for stage_file in \
		oberth-linux-amd64 \
		oberth-linux-arm64 \
		oberth-darwin-amd64 \
		oberth-darwin-arm64 \
		oberth-runner-linux-amd64 \
		oberth-runner-linux-arm64 \
		SHA256SUMS \
		SHA256SUMS.sigstore.json \
		cosign.pub; do
		stage_headers=$stage_dir/$stage_file.headers
		stage_status=$(authoritative_object_status "oberth/$stage_version/$stage_file" "$stage_dir/$stage_file" "$stage_headers")
		[ "$stage_status" = 200 ] || fail "versioned release $stage_version lacks $stage_file in authoritative R2"
		rm -f -- "$stage_headers"
	done
	cmp -s "$release_dir/cosign.pub" "$stage_dir/cosign.pub" || fail "versioned release $stage_version uses an untrusted signing key"
	(cd "$stage_dir" && sha256sum -c SHA256SUMS >/dev/null) || fail "versioned release $stage_version has invalid checksums"
	cosign verify-blob --key "$release_dir/cosign.pub" --bundle "$stage_dir/SHA256SUMS.sigstore.json" "$stage_dir/SHA256SUMS" >/dev/null || \
		fail "versioned release $stage_version has an invalid signature bundle"
	verify_binary_version "$stage_dir/$(native_binary)" "$stage_version"
)

converge_latest_aliases() {
	converge_attempt=1
	while [ "$converge_attempt" -le 10 ]; do
		converge_marker=$release_dir/.converge-VERSION
		converge_headers=$release_dir/.converge-VERSION.headers
		converge_status=$(authoritative_object_status oberth/latest/VERSION "$converge_marker" "$converge_headers")
		[ "$converge_status" = 200 ] || fail "latest VERSION disappeared during finalization"
		converge_version=$(read_marker_version "$converge_marker")
		converge_dir=$release_dir/.latest-target
		stage_versioned_release "$converge_version" "$converge_dir"
		for converge_file in \
			oberth-linux-amd64 \
			oberth-linux-arm64 \
			oberth-darwin-amd64 \
			oberth-darwin-arm64 \
			oberth-runner-linux-amd64 \
			oberth-runner-linux-arm64 \
			SHA256SUMS \
			SHA256SUMS.sigstore.json \
			cosign.pub; do
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
				oberth-runner-linux-amd64 \
				oberth-runner-linux-arm64 \
				SHA256SUMS \
				SHA256SUMS.sigstore.json \
				cosign.pub; do
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
				state=$(oberth-release-support chart-index-state "$current" "$chart_version" "$chart_digest" "$chart_url")
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
			helm repo index "$stage" --url "$chart_repo" --merge "$merge_argument"
		else
			helm repo index "$stage" --url "$chart_repo"
		fi
		oberth-release-support chart-index-state "$stage/index.yaml" "$chart_version" "$chart_digest" "$chart_url" | grep -Fx exact >/dev/null || \
			fail "generated chart index does not bind the exact release chart"
		put_status=$(put_conditionally oberth/index.yaml "$stage/index.yaml" 'text/yaml; charset=utf-8' "$condition")
		case "$put_status" in
			200|201)
				readback=$release_dir/.chart-index-readback
				readback_headers=$release_dir/.chart-index-readback.headers
				readback_status=$(authoritative_object_status oberth/index.yaml "$readback" "$readback_headers")
				[ "$readback_status" = 200 ] || fail "chart index readback returned HTTP $readback_status"
				oberth-release-support chart-index-state "$readback" "$chart_version" "$chart_digest" "$chart_url" | grep -Fx exact >/dev/null || \
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
	validate_source
	require_artifacts
	verify_release
	initialize_authoritative_r2
	advance_latest_marker >/dev/null
	converge_latest_aliases
	finalize_chart_index
	final_marker=$release_dir/.stable-VERSION
	final_headers=$release_dir/.stable-VERSION.headers
	final_status=$(authoritative_object_status oberth/latest/VERSION "$final_marker" "$final_headers")
	[ "$final_status" = 200 ] || fail "stable VERSION disappeared after finalization"
	stable_version=$(read_marker_version "$final_marker")
	printf 'Stable Oberth release is %s\n' "$stable_version"
}

case "$action" in
	validate) validate_source ;;
	build) build_artifacts ;;
	publish-r2) publish_r2_binaries ;;
	publish-images) publish_images ;;
	package-chart) prepare_chart ;;
	publish-chart) publish_chart ;;
	verify) verify_release ;;
	finalize) finalize_release ;;
	*) fail "usage: $0 validate|build|publish-r2|publish-images|package-chart|publish-chart|verify|finalize" ;;
esac
