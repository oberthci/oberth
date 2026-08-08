#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
exec "$script_dir/../charts/oberth/files/prepare-node.sh" "$@"
