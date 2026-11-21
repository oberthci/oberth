#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
source hack/steps.sh
if [[ -z "${OBERTH_PARITY_TARGET_FILE:-}" ]]; then
    printf 'parity: target absent; integration pending\n'
    exit 0
fi
: "${OBERTH_PARITY_ROOT:?Set the absolute path to the standalone parity checkout}"
: "${OBERTH_PARITY_TARGET_FILE:?Set the absolute path to the walk-prepared target file}"
[[ "$OBERTH_PARITY_ROOT" == /* && "$OBERTH_PARITY_TARGET_FILE" == /* ]] || {
    printf 'parity: checkout and target paths must be absolute\n' >&2
    exit 2
}
command -v parity >/dev/null || {
    printf 'parity: install the standalone parity executable on PATH\n' >&2
    exit 2
}
test -d "$OBERTH_PARITY_ROOT/oracle/fixtures"
test -r "$OBERTH_PARITY_TARGET_FILE"
cd "$OBERTH_PARITY_ROOT"
run parity replay --bundle --target-file "$OBERTH_PARITY_TARGET_FILE"
