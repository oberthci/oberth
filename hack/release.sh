#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
fail() { printf '%s\n' "$@" >&2; exit 1; }
operation=${1:-}
RELEASE_ORIGIN=${RELEASE_ORIGIN:-}
RELEASE_ORIGIN=${RELEASE_ORIGIN%/}
version=${VERSION:-}
version_pattern=$(cat internal/workflowpolicy/release-version.pattern) || fail 'Release version rule is missing; use the complete admitted source.'
[[ $version =~ $version_pattern ]] || fail 'Release tag is invalid; use vMAJOR.MINOR.PATCH.'
dir=${RELEASE_DIR:-/work/shared/dist}
work=${RELEASE_SHARED:-/work/shared}
private=${RELEASE_PRIVATE:-/run/private}
tool=${RELEASE_TOOL:-/work/shared/oberth-release}
case $operation in
    check)
        commit=$(git rev-parse HEAD)
        tagged=$(git rev-parse --verify "refs/tags/$version^{commit}" 2>/dev/null) || fail 'Release tag is missing; deliver its exact Git object.'
        [[ $tagged = "$commit" ]] || fail 'Release tag points elsewhere; release the admitted commit.'
        [[ ${RELEASE_BUILD_STATUS:-} = passed && ${RELEASE_TESTED_COMMIT:-} = "$commit" ]] || fail 'Release build facts are missing; push a tested release tag.'
        exit 0 ;;
    prepare-signing|sign|verify) ;;
    publish|finalize)
        [[ ${RELEASE_ORIGIN:-} = https://* ]] || fail 'Release origin is not HTTPS; configure an HTTPS origin.'
        [[ -s ${RELEASE_TOKEN_FILE:-} ]] || fail 'Release token is missing; provide the projected token file.'
        [[ ${RELEASE_EXCHANGE_URL:-} = https://* ]] || fail 'Token exchange is not HTTPS; configure an HTTPS endpoint.' ;;
    *) fail 'Release operation is invalid; use a documented release operation.' ;;
esac
if [[ $operation != sign && $operation != prepare-signing ]]; then
    expected_origin=$(sed -n 's/^const Origin = "\(.*\)"$/\1/p' internal/releaseverify/origin.go) || fail 'Release address is missing; use the complete admitted source.'
    [[ $RELEASE_ORIGIN = "$expected_origin" ]] || fail 'Release origin differs; use the public release address.'
fi
directory_failure() {
    fail 'Release directory is unusable; set the named path to a writable directory.' "Directory: $1=$2"
}
prepare_directory() {
    mkdir -p "$2" 2>/dev/null && [[ -w $2 && -x $2 ]] || directory_failure "$1" "$2"
}
case $operation in
    publish|finalize)
        [[ -n ${RELEASE_AUDIENCE:-} ]] || fail 'Release audience is missing; set RELEASE_AUDIENCE for token exchange.' ;;
esac
prepare_directory RELEASE_PRIVATE "$private"
chmod 700 "$private" 2>/dev/null || directory_failure RELEASE_PRIVATE "$private"
export HOME=$private/home
prepare_directory RELEASE_PRIVATE "$HOME"
check_cosign() {
    [[ -f $private/cosign ]] || fail 'Signing tool is missing.' 'Place the pinned executable at RELEASE_PRIVATE/cosign before signing offline.'
    printf '%s  %s\n' ae1ecd212663f3693ad9edf8b1a183900c9a52d3155ba6e354237f9a0f6463fc "$private/cosign" | sha256sum -c - >/dev/null 2>"$private/cosign.error" || fail 'Signing tool digest differs; fetch the pinned signing tool.' "Detail: $private/cosign.error"
    chmod 700 "$private/cosign"
}
exchange() {
    curl --proto '=https' --fail --silent --show-error --max-time 60 \
        --data-urlencode grant_type=urn:ietf:params:oauth:grant-type:token-exchange \
        --data-urlencode subject_token_type=urn:ietf:params:oauth:token-type:jwt \
        --data-urlencode "subject_token@$RELEASE_TOKEN_FILE" \
        --data-urlencode "audience=$RELEASE_AUDIENCE" \
        --output "$private/exchange.json" "$RELEASE_EXCHANGE_URL" 2>"$private/exchange.error" || fail 'Release token exchange failed; check the granted audience.'
    python3 - "$private/exchange.json" "$private/access.token" 2>/dev/null <<'PY' || fail 'Release token is invalid; request a short-lived bearer token.'
import json, pathlib, sys
value = json.loads(pathlib.Path(sys.argv[1]).read_text())
if (str(value.get("token_type", "")).lower() != "bearer" or type(value.get("expires_in")) is not int
        or not 0 < value["expires_in"] <= 3600 or not value.get("access_token")):
    sys.exit(1)
pathlib.Path(sys.argv[2]).write_text(value["access_token"])
PY
    chmod 600 "$private/access.token"
}
case $operation in
    prepare-signing)
        curl --proto '=https' --proto-redir '=https' --location --max-redirs 3 --fail --silent --show-error --retry 3 --max-time 60 \
            --output "$private/cosign" https://github.com/sigstore/cosign/releases/download/v3.1.1/cosign-linux-amd64 \
            2>"$private/cosign.error" || fail 'Signing tool download failed.' 'Check network access, then rerun hack/release.sh prepare-signing.' "Detail: $private/cosign.error"
        check_cosign ;;
    sign)
        key=${OBERTH_SECRETS_DIR:-}/oberth/data/release/signing
        [[ -n ${OBERTH_SECRETS_DIR:-} && -f $key && -r $key && -s $key ]] || fail 'Signing key file is missing; provide the signing-step grant.'
        check_cosign
        COSIGN_PASSWORD= "$private/cosign" public-key --key "$key" </dev/null >"$private/candidate.pub" 2>/dev/null || fail 'Signing key cannot be read; use a key with an empty password.'
        "$tool" key --public-key-file "$private/candidate.pub" >/dev/null
        printf '%s\n' '{"mediaType":"application/vnd.dev.sigstore.signingconfig.v0.2+json","caUrls":[],"oidcUrls":[],"rekorTlogUrls":[],"tsaUrls":[],"rekorTlogConfig":{"selector":"ANY"},"tsaConfig":{"selector":"ANY"}}' > "$private/signing-config.json"
        printf '%s\n' '{"mediaType":"application/vnd.dev.sigstore.trustedroot+json;version=0.1"}' > "$private/trusted-root.json"
        COSIGN_PASSWORD= "$private/cosign" sign-blob --yes --trusted-root "$private/trusted-root.json" --signing-config "$private/signing-config.json" --key "$key" --bundle "$dir/SHA256SUMS.sigstore.json" "$dir/SHA256SUMS" </dev/null >/dev/null 2>&1 || fail 'Signing failed; check the checksum file and release directory.'
        "$tool" signature --dir "$dir" ;;
    publish|finalize)
        prepare_directory RELEASE_SHARED "$work"
        exchange
        "$tool" "$operation" --dir "$dir" --version "$version" --origin "$RELEASE_ORIGIN" \
            --token-file "$private/access.token" --receipt "$work/verified.json" ;;
    verify)
        [[ ${RELEASE_ORIGIN:-} = https://* ]] || fail 'Release origin is not HTTPS; configure an HTTPS origin.'
        prepare_directory RELEASE_SHARED "$work"
        verify_dir=${RELEASE_VERIFY_DIR:-/verify}
        prepare_directory RELEASE_VERIFY_DIR "$verify_dir"
        clean=$(mktemp -d "$verify_dir/release.XXXXXX" 2>/dev/null) || directory_failure RELEASE_VERIFY_DIR "$verify_dir"
        for file in SHA256SUMS SHA256SUMS.sigstore.json; do
            curl --proto '=https' --fail --silent --show-error --max-time 60 --max-filesize 1048576 --output "$clean/$file" "$RELEASE_ORIGIN/$version/$file"
        done
        "$tool" signature --dir "$clean"
        while IFS=' ' read -r digest file; do
            [[ $digest =~ ^[0-9a-f]{64}$ && $file =~ ^[a-z0-9][a-z0-9._-]*$ ]] || fail 'Release checksum line is invalid; fetch the exact release again.'
            maximum=536870912
            case $file in release.json) maximum=1048576;; oberth.tgz) maximum=67108864;; esac
            curl --proto '=https' --fail --silent --show-error --max-time 60 --max-filesize "$maximum" --output "$clean/$file" "$RELEASE_ORIGIN/$version/$file"
        done < "$clean/SHA256SUMS"
        "$tool" verify --dir "$clean" --version "$version" --receipt "$work/verified.json" ;;
esac
