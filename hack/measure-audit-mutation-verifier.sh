#!/bin/sh
set -eu

# This measures only the authoritative local SQLite verifier. It intentionally
# excludes AuditHeadHint plus TSA and Kubernetes continuity callback latency.
if output="$(GOWORK=off OBERTH_AUDIT_METRICS=1 go test -run '^TestAuditMutation' -count=1 -v ./internal/store 2>&1)"; then
	:
else
	status=$?
	printf '%s\n' "$output" >&2
	exit "$status"
fi
json="$(printf '%s\n' "$output" | sed -n 's/^AUDIT_METRICS_JSON=//p' | tail -n 1)"
if [ -z "$json" ]; then
	printf '%s\n' "$output" >&2
	exit 1
fi
printf '%s\n' "$json"
