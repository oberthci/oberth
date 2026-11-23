#!/usr/bin/env bash
set -Eeuo pipefail
cd "${BASH_SOURCE[0]%/*}/.."
if ! command -v chromium >/dev/null; then
    printf 'ui-browser: SKIPPED (chromium is not on PATH)\n'
    exit 0
fi
if ! command -v node >/dev/null; then
    printf 'ui-browser: FAIL (node is required when chromium is available)\n' >&2
    exit 1
fi
export GOTMPDIR="${GOTMPDIR:-$HOME/.cache/go-tmp}"
export GOMAXPROCS=2
export OBERTH_UI_ARTIFACTS="${OBERTH_UI_ARTIFACTS:-$PWD/.cache/webui}"
mkdir -p "$GOTMPDIR" "$OBERTH_UI_ARTIFACTS"
export OBERTH_UI_BINARY="$OBERTH_UI_ARTIFACTS/api.test"
export CHROMIUM_BIN
CHROMIUM_BIN=$(command -v chromium)
printf 'ui-browser: RUN (chromium is available)\n'
export OBERTH_UI_STEP_FILE="$OBERTH_UI_ARTIFACTS/check-step"
printf 'compile example server' > "$OBERTH_UI_STEP_FILE"
if { timeout --signal=KILL 90s bash -ec '
    go test -c -p=1 -buildvcs=false -o "$OBERTH_UI_BINARY" ./internal/api
    printf "launch browser" > "$OBERTH_UI_STEP_FILE"
    node hack/webui/check.mjs "$@"
' ui-check "$@"; } 2> "$OBERTH_UI_ARTIFACTS/check-stderr"; then
    cat "$OBERTH_UI_ARTIFACTS/check-stderr" >&2
else
    result=$?
    if [[ $result == 137 ]]; then
        printf 'ui-browser: FAIL (90s limit: %s)\n' "$(< "$OBERTH_UI_STEP_FILE")" >&2
    else
        cat "$OBERTH_UI_ARTIFACTS/check-stderr" >&2
    fi
    exit "$result"
fi
printf 'ui-browser: PASS\n'
