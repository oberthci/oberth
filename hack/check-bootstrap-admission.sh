#!/bin/sh
# Check candidate pipeline YAML with the previous released server validator.
set -eu
if [ "$#" -gt 2 ]; then
  echo 'usage: check-bootstrap-admission.sh [candidate-root] [validator-ref]' >&2
  exit 2
fi
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
candidate=${1:-$root}
candidate=$(CDPATH='' cd -- "$candidate" && pwd)
# Default to v0.22.0, the deployed server that builds the next release.
validator=${2:-fa47f57704ff8b6e1daf5a1f128cff23a8328b76}
base=$(git -C "$root" rev-parse --verify --end-of-options "$validator^{commit}")
checkout=$(mktemp -d)
trap 'rm -rf "$checkout"' EXIT HUP INT TERM
# The archive is local and read-only with respect to the user's worktrees.
git -C "$root" archive --format=tar "$base" >"$checkout/source.tar"
mkdir "$checkout/source"
tar -xf "$checkout/source.tar" -C "$checkout/source"
cp "$root/hack/testdata/bootstrap-admission/admission_test.go" "$checkout/source/pkg/argoworkflow/bootstrap_repository_test.go"
printf 'Bootstrap admission: validator %s (%s) against %s/.oberth/*.yaml\n' "$validator" "$base" "$candidate"
cd "$checkout/source"
go test ./pkg/argoworkflow -run '^TestBootstrapRepositoryAdmission$' -count=1 -v -args "$candidate"
