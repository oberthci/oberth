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
        github_token=${OBERTH_SECRETS_DIR:-}/oberth/data/release/github-token
        [[ -n ${OBERTH_SECRETS_DIR:-} && -s $github_token ]] || fail 'GitHub release token is missing; grant it to this step.' ;;
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
prepare_directory RELEASE_PRIVATE "$private"
chmod 700 "$private" 2>/dev/null || directory_failure RELEASE_PRIVATE "$private"
export HOME=$private/home
prepare_directory RELEASE_PRIVATE "$HOME"
check_cosign() {
    [[ -f $private/cosign ]] || fail 'Signing tool is missing.' 'Place the pinned executable at RELEASE_PRIVATE/cosign before signing offline.'
    printf '%s  %s\n' ae1ecd212663f3693ad9edf8b1a183900c9a52d3155ba6e354237f9a0f6463fc "$private/cosign" | sha256sum -c - >/dev/null 2>"$private/cosign.error" || fail 'Signing tool digest differs; fetch the pinned signing tool.' "Detail: $private/cosign.error"
    chmod 700 "$private/cosign"
}
# GitHub answers a release download with a redirect to its asset host; follow
# at most the canonical count, each over HTTPS under the origin or to an asset host.
redirect_allowed() {
    [[ $1 = "$RELEASE_ORIGIN"/* && $1 != *..* ]] && return 0
    local host
    for host in $(sed -n 's/^const AssetHosts = "\(.*\)"$/\1/p' internal/releaseverify/origin.go); do
        [[ $1 = "https://$host/"* ]] && return 0
    done
    return 1
}
download() {
    local address=$RELEASE_ORIGIN/$version/$1 hops=0 reply limit
    limit=$(sed -n 's/^const MaxRedirects = \([0-9]*\)$/\1/p' internal/releaseverify/origin.go)
    while :; do
        reply=$(curl --proto '=https' --fail --silent --show-error --max-time 60 --max-filesize "$2" --max-redirs 0 \
            --write-out '%{http_code} %{redirect_url}' --output "$clean/$1" "$address") || fail 'Release download failed; check the release address and version.'
        [[ ${reply%% *} = 3* ]] || return 0
        address=${reply#* }
        hops=$((hops + 1))
        (( hops <= limit )) && redirect_allowed "$address" || fail 'Release download redirected elsewhere; report the redirect.' "Target: $address"
    done
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
        "$tool" "$operation" --dir "$dir" --version "$version" --origin "$RELEASE_ORIGIN" \
            --token-file "$github_token" --receipt "$work/verified.json" ;;
    verify)
        [[ ${RELEASE_ORIGIN:-} = https://* ]] || fail 'Release origin is not HTTPS; configure an HTTPS origin.'
        prepare_directory RELEASE_SHARED "$work"
        verify_dir=${RELEASE_VERIFY_DIR:-/verify}
        prepare_directory RELEASE_VERIFY_DIR "$verify_dir"
        clean=$(mktemp -d "$verify_dir/release.XXXXXX" 2>/dev/null) || directory_failure RELEASE_VERIFY_DIR "$verify_dir"
        for file in SHA256SUMS SHA256SUMS.sigstore.json; do
            download "$file" 1048576
        done
        "$tool" signature --dir "$clean"
        while IFS=' ' read -r digest file; do
            [[ $digest =~ ^[0-9a-f]{64}$ && $file =~ ^[a-z0-9][a-z0-9._-]*$ ]] || fail 'Release checksum line is invalid; fetch the exact release again.'
            maximum=536870912
            case $file in release.json) maximum=1048576;; oberth.tgz) maximum=67108864;; esac
            download "$file" "$maximum"
        done < "$clean/SHA256SUMS"
        "$tool" verify --dir "$clean" --version "$version" --receipt "$work/verified.json" ;;
esac
