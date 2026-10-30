#!/bin/sh
# golangci-lint's analysis workers do not share its --timeout context.
# Keep a process-group deadline outside the tool, with a bounded TERM grace.
set -eu
# The server supplies OBERTH_CACHE_DIR only with a repository/tier-scoped,
# node-local cache mount. Never write into the read-only setup tool directory.
# v2.12.2 uses Go's multi-process-safe disk cache; isolate tool versions.
export GOLANGCI_LINT_CACHE="${OBERTH_CACHE_DIR:-${TMPDIR:-/tmp}}/golangci-lint-v2.12.2"
exec /usr/bin/timeout --signal=TERM --kill-after=15s 12m "$@"
