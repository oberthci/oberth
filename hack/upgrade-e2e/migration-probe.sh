#!/usr/bin/env bash
# Exercise a real additional migration in an isolated source mutation.
set -euo pipefail
here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source_tree=${1:?source tree}
baseline_fixture=${2:?published fixture executable}
target_schema=${3:?target schema version}
runner_image=${4:?runner image}
evidence=${5:?evidence directory}
private=$(mktemp -d /tmp/oberth-migration-probe.XXXXXXXX)
trap 'find "$private" -delete' EXIT
mkdir -p "$private/source" "$private/data"
cp -a "$source_tree/." "$private/source/"
next=$((target_schema+1))
sed -E -i "s/^([[:space:]]*latestMigrationVersion[[:space:]]*=[[:space:]]*).*/\1$next/" "$private/source/internal/store/schema.go"
cp "$here/migration_probe.go.txt" "$private/source/internal/store/upgrade_e2e_probe.go"
cp "$here/migration_probe_test.go.txt" "$private/source/internal/store/upgrade_e2e_probe_test.go"
diff -u "$source_tree/internal/store/schema.go" "$private/source/internal/store/schema.go" >"$evidence/migration-probe-source.diff" || [[ $? == 1 ]]
"$baseline_fixture" seed "$private/data/oberth.sqlite" "$runner_image" >"$evidence/migration-probe-workflow.json"
"$baseline_fixture" snapshot "$private/data/oberth.sqlite" >"$evidence/migration-probe-before.json"
started=$(date +%s%N)
export E2E_PROBE_DATABASE="$private/data/oberth.sqlite"
(cd "$private/source" && go test -p=2 -parallel=2 -count=1 ./internal/store -run '^TestUpgradeE2EAddedMigration$' -timeout 2m) >"$evidence/migration-probe-apply.log" 2>&1
# The published reader must refuse this newer schema, demonstrating why each
# checkpoint requires a reader built from the target rather than the baseline.
if "$baseline_fixture" snapshot "$private/data/oberth.sqlite" >"$evidence/migration-probe-old-reader-red.log" 2>&1; then
  echo 'published snapshot reader unexpectedly accepted the newer schema' >&2; exit 1
fi
rg -i 'schema|version|migration' "$evidence/migration-probe-old-reader-red.log" >/dev/null
(cd "$private/source" && go build -p=2 -trimpath -buildvcs=false -o "$private/fixture" ./hack/upgrade-e2e/fixture)
"$private/fixture" snapshot "$private/data/oberth.sqlite" >"$evidence/migration-probe-after.json"
"$baseline_fixture" verify "$evidence/migration-probe-before.json" "$evidence/migration-probe-after.json" "$next" "$started"
(cd "$private/source" && go test -p=2 -parallel=2 -count=1 ./internal/store -run '^TestUpgradeE2EAddedMigration$' -timeout 2m) >"$evidence/migration-probe-reopen.log" 2>&1
"$private/fixture" snapshot "$private/data/oberth.sqlite" >"$evidence/migration-probe-reopen.json"
"$baseline_fixture" verify "$evidence/migration-probe-after.json" "$evidence/migration-probe-reopen.json" "$next" "$started"
printf 'PASS: isolated schema mutation to %s applied once; old rows and reopen unchanged\n' "$next"
