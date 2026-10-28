#!/bin/sh
# The complete native Git cache suite remains a required source gate.
set -eu
go test -race -count=1 ./internal/gitcache
