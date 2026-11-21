#!/bin/sh
# Provision the canonical public key and the standalone fetcher together.
set -eu
usage() { printf '%s\n' 'Use hack/release-pin.sh [--replace] <public-key-file>.' >&2; exit 1; }
replace=false
if [ "$#" -gt 0 ] && [ "$1" = --replace ]; then replace=true; shift; fi
[ "$#" -eq 1 ] || usage
case $1 in -*) usage;; esac
root=$(dirname "$(dirname "$(realpath "$0")")")
export GOTMPDIR="${GOTMPDIR:-$HOME/.cache/go-tmp}"
mkdir -p "$GOTMPDIR"
key=$1
case $key in /*) ;; *) key=$PWD/$key ;; esac
build=$(mktemp -d "$GOTMPDIR/release-pin.XXXXXXXX")
trap 'find "$build" -depth -delete' 0
go -C "$root" build -p=2 -buildvcs=false -o "$build/oberth-release" ./cmd/oberth-release
cd "$root"
"$build/oberth-release" pin --public-key-file "$key" --replace="$replace"
