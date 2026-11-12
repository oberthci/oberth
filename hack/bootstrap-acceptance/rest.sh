#!/bin/sh
# One race run supplies the contract's pass events; no extra fixture compile.
set -eu
exec /usr/bin/python3 -I -B hack/bootstrap-acceptance/ci.py "$@"
