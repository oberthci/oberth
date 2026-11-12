#!/bin/sh
# Check candidate pipeline YAML with the exact server that must build v0.22.0.
set -eu
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
candidate=${1:-$root}
candidate=$(CDPATH='' cd -- "$candidate" && pwd)
base=e5421125df457ba4ce08d72467069c2d47187a21
checkout=$(mktemp -d)
trap 'rm -rf "$checkout"' EXIT HUP INT TERM
# The archive is local and read-only with respect to the user's worktrees.
git -C "$root" archive --format=tar "$base" >"$checkout/source.tar"
mkdir "$checkout/source"
tar -xf "$checkout/source.tar" -C "$checkout/source"
cp "$root/hack/testdata/bootstrap-admission/admission_test.go" "$checkout/source/pkg/argoworkflow/bootstrap_repository_test.go"
printf 'Bootstrap admission: v0.21.10 (%s) against %s/.oberth/*.yaml\n' "$base" "$candidate"
cd "$checkout/source"
go test ./pkg/argoworkflow -run '^TestBootstrapRepositoryAdmission$' -count=1 -v -args "$candidate"
