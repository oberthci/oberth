#!/bin/sh
# The complete native Git cache suite remains a required source gate.
set -eu
/bin/sh hack/test-install-static-git-packages.sh
go test -race -count=1 ./internal/gitcache
