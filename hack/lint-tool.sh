#!/usr/bin/env bash
set -Eeuo pipefail
exec python3 -I "$(dirname "$0")/tools/go_tool.py" lint "$@"
