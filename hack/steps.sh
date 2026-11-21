#!/usr/bin/env bash
# Shared command reporting and on-disk temporary space for tooling.
set -Eeuo pipefail
failure() {
    printf "FAIL (%s): %s\n" "$1" "$2" >&2
    exit "$1"
}
trap 'failure "$?" "$BASH_COMMAND"' ERR
export GOTMPDIR="${GOTMPDIR:-$HOME/.cache/go-tmp}"
mkdir -p "$GOTMPDIR"
export TMPDIR="$GOTMPDIR"
export GOFLAGS="${GOFLAGS:-} -p=2 -buildvcs=false"
run() {
    printf '+'
    printf ' %q' "$@"
    printf '\n'
    local started=$SECONDS status=0
    "$@" || status=$?
    printf 'elapsed: %ss\n' "$((SECONDS - started))"
    if ((status)); then
        printf "FAIL (%s):" "$status" >&2
        printf " %q" "$@" >&2
        printf "\n" >&2
        exit "$status"
    fi
}

# Top-level boundaries include lint preparation and the unwrapped guards step.
if [[ ${BASH_SOURCE[1]:-} == hack/check.sh || ${BASH_SOURCE[1]:-} == */hack/check.sh ]]; then
    trap 'printf "check-clock: %ss\n" "$SECONDS"' DEBUG EXIT
fi
