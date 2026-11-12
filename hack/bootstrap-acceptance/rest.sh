#!/bin/sh
# Both branch and release rest leaves stop here if the bootstrap contract fails.
set -eu
/usr/bin/python3 -I -B hack/bootstrap-acceptance/ci.py
exec "$@"
