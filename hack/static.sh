#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
names=${OBERTH_SECRETS_DIR:-}/oberth/policy/names
[[ -n ${OBERTH_SECRETS_DIR:-} && -s $names && -f $names ]] || { echo 'Names policy is missing; store and grant it for this step.' >&2; exit 1; }
unset OBERTH_SECRETS_DIR OBERTH_GUARDS_NAMES_FILE
git diff --name-only "$BUILD_DEFAULT_REF" HEAD -- Makefile hack .oberth
GOLANGCI_LINT="$PWD/hack/lint-tool.sh" make lint
OBERTH_GUARDS_NAMES_FILE="$names" make guards
python3 hack/plans/parity.py --check
