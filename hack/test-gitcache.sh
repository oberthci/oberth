#!/bin/sh
# The complete native Git cache suite remains a required source gate.
set -eu
/bin/sh hack/test-install-static-git-packages.sh
exec python3 -I -S -B .oberth/test-packages.py gitcache
