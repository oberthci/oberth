#!/usr/bin/env bash
# Publish a signed release directory as the assets of the tag's GitHub release,
# by hand, as the release pipeline's publish and finalize steps do:
#
#   hack/publish-github-release.sh [--dry-run] <tag> <release-dir> <token-file>
#
# The tag must already be on GitHub. Assets already there must hold the same
# bytes; nothing is replaced or deleted. Files go into a draft, the manifest
# last, and the release becomes latest only when its version is newer than the
# current latest. The token is read from <token-file> into a private header
# file for curl; it never reaches a command line, the output or a log.
# --dry-run reads GitHub and prints each write instead of making it.
# This checks no signature: run oberth-release verify on the directory first.
set -Eeuo pipefail
fail() { printf '%s\n' "$@" >&2; exit 1; }
dry_run=no
if [[ ${1:-} = --dry-run ]]; then dry_run=yes; shift; fi
(( $# == 3 )) || fail 'Publication arguments are invalid.' 'Run hack/publish-github-release.sh [--dry-run] <tag> <release-dir> <token-file>.'
tag=$1 dir=${2%/} token_file=$3
root=$(dirname "$0")/..
pattern=$(cat "$root/internal/workflowpolicy/release-version.pattern") || fail 'Release version rule is missing; use the complete source tree.'
[[ $tag =~ $pattern ]] || fail 'Release tag is invalid; use vMAJOR.MINOR.PATCH.'
for name in release.json SHA256SUMS SHA256SUMS.sigstore.json; do
    [[ -f $dir/$name && -s $dir/$name ]] || fail 'Release directory lacks signed files; pass a signed release directory.' "Missing: $dir/$name"
done
[[ -f $token_file && -s $token_file ]] || fail 'GitHub token file is missing or empty; pass the token file path.'
origin=$(sed -n 's/^const Origin = "\(.*\)"$/\1/p' "$root/internal/releaseverify/origin.go")
repository=${origin#https://github.com/}
repository=${repository%/releases/download}
[[ $repository =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fail 'Release origin is invalid; use the complete source tree.'
api=https://api.github.com/repos/$repository
uploads=https://uploads.github.com/repos/$repository

private=$(umask 077 && mktemp -d) || fail 'Private scratch is unavailable; check TMPDIR.'
cleanup() { rm -f -- "$private/headers" "$private/answer"; rmdir -- "$private"; }
trap cleanup EXIT
token=$(<"$token_file")
token=${token//[[:space:]]/}
[[ -n $token ]] || fail 'GitHub token file is missing or empty; pass the token file path.'
printf 'Authorization: Bearer %s\nAccept: application/vnd.github+json\nX-GitHub-Api-Version: 2022-11-28\n' "$token" > "$private/headers"
unset token

# call METHOD URL [curl arguments] leaves the answer in $private/answer and
# prints the HTTP status. Redirects are not followed.
call() {
    local method=$1 url=$2
    shift 2
    curl --proto '=https' --silent --show-error --max-time 900 --header @"$private/headers" \
        --request "$method" --output "$private/answer" --write-out '%{http_code}' "$@" "$url" ||
        fail 'GitHub could not be reached; check the network, then retry.'
}
refused() { fail 'GitHub refused the release request.' "Detail: GitHub answered $1 to $2"; }
# json FIELD prints a field of the last answer; json assets prints one line
# per asset and json draft TAG the id of that tag's draft in a release list.
json() {
    python3 -I -c '
import json, sys
value, field = json.load(open(sys.argv[1])), sys.argv[2]
if field == "assets":
    for asset in value["assets"]:
        print(asset["name"], asset["size"], asset["state"], asset.get("digest") or "-")
elif field == "draft":
    print(next((r["id"] for r in value or [] if r["draft"] and r["tag_name"] == sys.argv[3]), ""))
else:
    print(value[field])
' "$private/answer" "$@"
}
# write describes a change and makes it unless this is a dry run.
write() {
    local what=$1 method=$2 url=$3 want=$4 status
    shift 4
    if [[ $dry_run = yes ]]; then
        printf 'Would %s.\n' "$what"
        return 0
    fi
    status=$(call "$method" "$url" "$@")
    [[ $status = "$want" ]] || refused "$status" "$method $url"
    printf '%s.\n' "${what^}"
}
json_body=(--header 'Content-Type: application/json')
newer() {
    local a=${1#v} b=${2#v} i x y
    for i in 1 2 3; do
        x=${a%%.*} y=${b%%.*}
        (( x > y )) && return 0
        (( x < y )) && return 1
        a=${a#*.} b=${b#*.}
    done
    return 1
}

names=()
for path in "$dir"/*; do
    name=${path##*/}
    [[ -d $path && $name = *.oci ]] && continue
    [[ -f $path && $name =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || fail 'Release directory holds an unexpected entry; pass the signed release directory.' "Entry: $path"
    case $name in release.json|SHA256SUMS|SHA256SUMS.sigstore.json) ;; *) names+=("$name") ;; esac
done
names+=(release.json SHA256SUMS SHA256SUMS.sigstore.json)

status=$(call GET "$api/git/ref/tags/$tag")
[[ $status = 200 ]] || { [[ $status = 404 ]] && fail 'Release tag is not on GitHub.' 'Push the release tag to GitHub, then retry publication.'; refused "$status" "GET tag $tag"; }

# The tag's release, a draft left by an earlier attempt, or a new draft. A
# draft is not reachable by its tag, so it is looked for among the newest.
id='' draft=true assets=''
status=$(call GET "$api/releases/tags/$tag")
if [[ $status = 200 ]]; then
    id=$(json id) draft=false
else
    [[ $status = 404 ]] || refused "$status" "GET release $tag"
    status=$(call GET "$api/releases?per_page=10")
    [[ $status = 200 ]] || refused "$status" "GET releases"
    id=$(json draft "$tag")
    if [[ -n $id ]]; then
        status=$(call GET "$api/releases/$id")
        [[ $status = 200 ]] || refused "$status" "GET release $id"
    fi
fi
if [[ -n $id ]]; then
    assets=$(json assets)
else
    write "create a draft release for $tag" POST "$api/releases" 201 "${json_body[@]}" --data "{\"tag_name\":\"$tag\",\"name\":\"$tag\",\"draft\":true}"
    id=$([[ $dry_run = yes ]] && printf new || json id)
fi

uploads_needed=()
for name in "${names[@]}"; do
    line=$(printf '%s\n' "$assets" | awk -v name="$name" '$1 == name')
    if [[ -z $line ]]; then
        uploads_needed+=("$name")
        continue
    fi
    read -r _ size state digest <<<"$line"
    [[ $state = uploaded && $digest = sha256:* ]] || fail 'Published release asset cannot be confirmed.' 'Check the named asset on GitHub; delete it only if its upload is incomplete.' "Asset: $name"
    local_digest=$(sha256sum "$dir/$name") local_size=$(($(wc -c < "$dir/$name")))
    [[ $size = "$local_size" && $digest = "sha256:${local_digest%% *}" ]] || fail 'Published release bytes differ.' 'Use the existing identical release or publish a new version.' "Asset: $name"
    printf 'Keeps %s: the same bytes are published.\n' "$name"
done
for name in "${uploads_needed[@]}"; do
    write "upload $name" POST "$uploads/releases/$id/assets?name=$name" 201 \
        --header 'Content-Type: application/octet-stream' --data-binary @"$dir/$name"
done

latest=''
status=$(call GET "$api/releases/latest")
if [[ $status = 200 ]]; then
    latest=$(json tag_name)
    [[ $latest =~ $pattern ]] || fail "GitHub's latest release has an invalid tag." 'Mark a vMAJOR.MINOR.PATCH release as latest on GitHub, then retry.'
else
    [[ $status = 404 ]] || refused "$status" "GET latest release"
fi
make_latest=false
if [[ -z $latest ]] || newer "$tag" "$latest"; then make_latest=true; fi
if [[ $draft = true ]]; then
    write "publish $tag (latest: $make_latest)" PATCH "$api/releases/$id" 200 "${json_body[@]}" --data "{\"draft\":false,\"make_latest\":\"$make_latest\"}"
elif [[ $make_latest = true ]]; then
    write "mark $tag as latest" PATCH "$api/releases/$id" 200 "${json_body[@]}" --data '{"make_latest":"true"}'
fi
printf 'Release %s: https://github.com/%s/releases/tag/%s (latest: %s)\n' "$tag" "$repository" "$tag" "$([[ $make_latest = true ]] && printf '%s' "$tag" || printf '%s' "$latest")"
