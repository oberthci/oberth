#!/usr/bin/env bash
# A finite environment capture must be drained under pipefail.
set -euo pipefail

input=${1:?usage: check.sh ENVIRON_FILE}
if [[ ! -f "$input" || ! -r "$input" ]]; then
  printf '%s\n' 'FAIL: environment capture is not a readable file' >&2
  exit 1
fi
if tr '\0' '\n' <"$input" | grep 'PGPASSWORD' >/dev/null; then
  printf '%s\n' 'FAIL: PGPASSWORD found in captured process environment' >&2
  exit 1
else
  pipeline_status=("${PIPESTATUS[@]}")
  if [[ ${pipeline_status[0]} -ne 0 || ${pipeline_status[1]} -ne 1 ]]; then
    printf '%s\n' 'FAIL: environment capture could not be inspected' >&2
    exit 1
  fi
fi
printf '%s\n' 'PASS: secret not in captured process environment'
