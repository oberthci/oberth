#!/usr/bin/env bash
# Source-bound contract test for examples/environment-guard/check.sh.
# The environment assertion must drain finite environment output so pipefail
# cannot turn a detected secret into a false PASS.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

guard="$root/examples/environment-guard/check.sh"
payload="$work/environ.txt"

printf '%s\n' 'PGPASSWORD=s3cret' >"$payload"
awk 'BEGIN { for (i = 1; i <= 32; i++) printf "%10000s\n", "" }' >>"$payload"
negative="$work/negative.log"
if /bin/bash "$guard" "$payload" >"$negative" 2>&1; then
  printf '%s\n' 'environment-guard accepted a secret in /proc/1/environ' >&2
  exit 1
fi
grep -Fq 'FAIL: PGPASSWORD found' "$negative"

printf '%s\n' clean >"$payload"
positive="$work/positive.log"
/bin/bash "$guard" "$payload" >"$positive" 2>&1
grep -Fq 'PASS: secret not in captured process environment' "$positive"

if /bin/bash "$guard" "$work/missing" >"$work/missing.log" 2>&1; then
  printf '%s\n' 'environment-guard accepted a missing capture' >&2
  exit 1
fi
grep -Fq 'FAIL: environment capture is not a readable file' "$work/missing.log"

printf '%s\n' 'environment-guard pipefail contract: OK'
