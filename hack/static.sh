#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
names=${OBERTH_SECRETS_DIR:-}/oberth/policy/names
identity=${OBERTH_SECRETS_DIR:-}/oberth/policy/identity
[[ -n ${OBERTH_SECRETS_DIR:-} && -s $names && -f $names ]] || { echo 'Names policy is missing; store and grant it for this step.' >&2; exit 1; }
# The guards check history against the repository identity; a step has no Git config of its own.
author=()
if [[ -f $identity ]]; then
    line=$(<"$identity") pattern='^([^<>]*[^<> ]) <([^<> ]+)>$' count=${GIT_CONFIG_COUNT:-0}
    [[ $line != *$'\n'* && $line =~ $pattern ]] || { echo 'Identity must be one "Name <email>" line; store it again.' >&2; exit 1; }
    author=("GIT_CONFIG_COUNT=$((count + 2))" "GIT_CONFIG_KEY_$count=user.name" "GIT_CONFIG_VALUE_$count=${BASH_REMATCH[1]}"
        "GIT_CONFIG_KEY_$((count + 1))=user.email" "GIT_CONFIG_VALUE_$((count + 1))=${BASH_REMATCH[2]}")
fi
unset OBERTH_SECRETS_DIR OBERTH_GUARDS_NAMES_FILE
git diff --name-only "$BUILD_DEFAULT_REF" HEAD -- Makefile hack .oberth
GOLANGCI_LINT="$PWD/hack/lint-tool.sh" make lint
env "${author[@]}" OBERTH_GUARDS_NAMES_FILE="$names" make guards
python3 hack/plans/parity.py --check
