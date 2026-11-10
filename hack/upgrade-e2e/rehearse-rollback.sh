# shellcheck shell=bash
# shellcheck disable=SC2154 # run.sh defines the shared variables and helpers.
# Sourced by run.sh --rehearse-rollback; uses its variables and helpers.
#
# Rehearses the product's own upgrade and rollback commands across a real
# schema migration on the throwaway k3d cluster run.sh created:
#
#   published release --helm--> candidate-1 (the candidate as committed)
#   candidate-1 --oberth upgrade--> rehearsal (candidate + one throwaway
#                                    migration, this build only)
#   rehearsal --oberth upgrade --rollback--> candidate-1
#
# and checks that the previous version and the pre-upgrade history are back
# and that no run of the backup is dispatched again.

# The rehearsal migration exists only in the archived source tree of the
# rehearsal build; it is never part of a product commit.
rehearsal_migration=29991231235959-upgrade-e2e-rehearsal

# rehearsal_sources stamps exact semantic versions into the two candidate
# charts (the source chart is unversioned, release.sh stamps it at package
# time) and adds the throwaway migration to the rehearsal tree.
rehearsal_sources() {
  local major minor sha=${candidate_shas[candidate-1]}
  IFS=. read -r major minor _ <<<"$from"
  candidate_versions[candidate-1]=v$major.$((minor+1)).0-e2e.a.${sha:0:8}
  candidate_versions[rehearsal]=v$major.$((minor+1)).0-e2e.b.${sha:0:8}
  local version
  for version in candidate-1 rehearsal; do
    sed -E -i -e "s/^version:.*/version: ${candidate_versions[$version]#v}/" \
      -e "s/^appVersion:.*/appVersion: \"${candidate_versions[$version]}\"/" \
      "$private/source-$version/charts/oberth/Chart.yaml"
  done
  printf 'CREATE TABLE upgrade_e2e_rehearsal (id INTEGER PRIMARY KEY);\n' \
    >"$private/source-rehearsal/internal/store/migrations/$rehearsal_migration.sql"
  printf 'candidate-1=%s\nrehearsal=%s\n' "${candidate_versions[candidate-1]}" "${candidate_versions[rehearsal]}" >"$evidence/rehearsal-versions.txt"
}

# rehearsal_runs writes every run (status, phase, workflow, reason) and the
# schema of the live database to $1.
rehearsal_runs() {
  fixture_pod "runs-$1" runs
  k -n oberth logs "runs-$1" >"$evidence/rehearsal/runs-$1.jsonl"
  k -n oberth delete pod "runs-$1" --wait=true
}

# rehearsal_release_hold clears the admission hold with the fixture of the
# running build ($1), the server stopped, and starts the server again.
rehearsal_release_hold() {
  k -n oberth scale deploy/oberth --replicas=0
  k -n oberth wait --for=delete pod -l app.kubernetes.io/name=oberth --timeout=100s
  fixture_pod "hold-off-$1" hold-off "$2"
  k -n oberth delete pod "hold-off-$1" --wait=true
  k -n oberth scale deploy/oberth --replicas=1
  k -n oberth rollout status deploy/oberth --timeout=180s
}

rehearsal_version() {
  local want=$1 i
  for ((i=0;i<60;i++)); do
    if k -n oberth exec deploy/oberth -c oberth -- /usr/local/bin/oberth version >"$evidence/rehearsal/version-$2.txt" 2>/dev/null &&
      rg -qF "$want " "$evidence/rehearsal/version-$2.txt"; then
      return 0
    fi
    sleep 2
  done
  echo "server does not report $want" >&2
  return 1
}

rehearse_rollback() {
  local a=candidate-1 b=rehearsal out="$evidence/rehearsal" home="$private/home"
  local cli="$private/oberth-$b" schema_a schema_b record holder target started
  mkdir -p "$out" "$home"
  schema_a=$("$private/oberth-$a" version --schema)
  schema_b=$("$private/oberth-$b" version --schema)
  [[ $schema_b == $((schema_a+1)) ]] || { echo "rehearsal schema $schema_b is not $schema_a+1" >&2; return 1; }

  # 1. The candidate as committed, deployed as the default path does.
  "$helm" upgrade oberth "$private/source-$a/charts/oberth" -n oberth --reuse-values \
    --set-string "image.ref=${candidate_refs[$a]}" --set image.pullPolicy=Never --wait --timeout 180s
  k -n oberth rollout status deploy/oberth --timeout=180s
  rehearsal_version "${candidate_versions[$a]}" before
  k -n oberth-argo wait --for=jsonpath='{.status.phase}'=Running workflow/oberth-e2e-running --timeout=60s
  rehearsal_runs before
  jq -se --argjson s "$schema_a" '(map(select(.schema))|.[0].schema)==$s and ([.[]|select(.status=="running")]|length)==1 and ([.[]|select(.status=="queued")]|length)==4' \
    "$out/runs-before.jsonl" >/dev/null

  # 2. oberth upgrade: the product command, with its own backup and record.
  # The rehearsal chart is local and unsigned (--chart); signature
  # verification is covered by the unit tests on the published record.
  awk -v ref="${candidate_refs[$b]}" '/^image:/{img=1} img && /^  ref:/{print "  ref: " ref; img=0; next} {print}' \
    "$private/source-$b/charts/oberth/values.yaml" >"$private/rehearsal-values.yaml"
  mv "$private/rehearsal-values.yaml" "$private/source-$b/charts/oberth/values.yaml"
  rg -qF "ref: ${candidate_refs[$b]}" "$private/source-$b/charts/oberth/values.yaml"
  # A private HOME: the backup key and client configuration of the operator
  # running this rehearsal are never read or written.
  HOME=$home nice "$cli" upgrade --chart "$private/source-$b/charts/oberth" --yes --backup-dir "$out" \
    --set image.pullPolicy=Never --timeout 5m >"$out/upgrade.log" 2>&1 || { cat "$out/upgrade.log" >&2; return 1; }
  rehearsal_version "${candidate_versions[$b]}" upgraded
  record=$(find "$out" -maxdepth 1 -name 'oberth-upgrade-*.json' -print -quit)
  [[ -n $record ]] || { echo "oberth upgrade wrote no rollback record" >&2; return 1; }
  jq -e --argjson a "$schema_a" --argjson b "$schema_b" '.schemaBefore==$a and .schemaAfter==$b and (.backup|length)>0' "$record" >/dev/null
  test -s "$(jq -er .backup "$record")"
  rehearsal_runs upgraded
  jq -se --argjson s "$schema_b" '(map(select(.schema))|.[0].schema)==$s' "$out/runs-upgraded.jsonl" >/dev/null

  # 3. Work that happens on the new version: the holder's Workflow ends, the
  # hold is released, and the upgraded server claims the backup's queued runs.
  # (The fixture's local upstream is refused since v0.20, so a claimed run
  # fails before submission and is retried once: claimed all the same.)
  holder=$(jq -sr '.[]|select(.status=="running")|.id' "$out/runs-before.jsonl")
  target=$(jq -sr '[.[]|select(.status=="queued")]|.[0].id' "$out/runs-before.jsonl")
  k -n oberth-argo delete workflow oberth-e2e-running --wait=true
  rehearsal_release_hold upgraded "${candidate_refs[$b]}"
  for ((i=0;i<45;i++)); do
    rehearsal_runs dispatched
    if jq -se '[.[]|select(.id)]|all(.status!="queued")' "$out/runs-dispatched.jsonl" >/dev/null; then break; fi
    sleep 2
  done
  jq -se --arg id "$target" '[.[]|select(.id==$id)]|.[0].status!="queued"' "$out/runs-dispatched.jsonl" >/dev/null ||
    { echo "the upgraded server did not claim queued run $target" >&2; return 1; }

  # 4. oberth upgrade --rollback: Helm revisions back, then the backup.
  started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  HOME=$home nice "$cli" upgrade --rollback "$record" --yes --timeout 5m >"$out/rollback.log" 2>&1 || { cat "$out/rollback.log" >&2; return 1; }
  rehearsal_version "${candidate_versions[$a]}" rolled-back
  k -n oberth rollout status deploy/oberth --timeout=180s
  # Give the restored server's scheduler time to dispatch, if it would.
  sleep 45

  # 5. The previous version runs, the history is the backup's, and no run of
  # the backup was dispatched again.
  rehearsal_runs rolled-back
  jq -se --argjson s "$schema_a" '(map(select(.schema))|.[0].schema)==$s' "$out/runs-rolled-back.jsonl" >/dev/null
  jq -se --slurpfile before <(jq -s 'map(select(.id))|map(.id)|sort' "$out/runs-before.jsonl") \
    'map(select(.id))|map(.id)|sort == $before[0]' "$out/runs-rolled-back.jsonl" >/dev/null ||
    { echo "restored run set differs from the backup's" >&2; return 1; }
  jq -se '[.[]|select(.id)]|all(.status=="interrupted")' "$out/runs-rolled-back.jsonl" >/dev/null ||
    { echo "a restored run is still active" >&2; return 1; }
  jq -se --arg holder "$holder" '[.[]|select(.id and .id!=$holder)]|all(.reason|contains("restored from a backup; not dispatched again"))' \
    "$out/runs-rolled-back.jsonl" >/dev/null || { echo "a restored queued run lacks the restore reason" >&2; return 1; }
  jq -se --arg holder "$holder" '[.[]|select(.id==$holder)]|.[0]|(.error|contains("restored without re-executing it")) or (.reason|contains("restored from a backup"))' \
    "$out/runs-rolled-back.jsonl" >/dev/null || { echo "the restored running run was not interrupted by the restore" >&2; return 1; }
  # No Workflow created for any of these runs since the rollback began, and
  # none of the run the upgraded server admitted is left behind.
  k -n oberth-argo get workflows -o json >"$out/workflows-rolled-back.json"
  jq -e --arg since "$started" '[.items[]|select(.metadata.labels."oberth.ci/run"!=null and .metadata.creationTimestamp>=$since)]|length==0' \
    "$out/workflows-rolled-back.json" >/dev/null || { echo "a restored run was dispatched again" >&2; return 1; }
  # The restored database holds admission again (the backup's hold).
  # Release it and restart: had the restored queued runs been left queued,
  # this would claim them, fail them and add retries.
  started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  rehearsal_release_hold rolled-back "${candidate_refs[$a]}"
  sleep 45
  k -n oberth-argo get workflows -o json >"$out/workflows-hold-released.json"
  jq -e --arg since "$started" '[.items[]|select(.metadata.labels."oberth.ci/run"!=null and .metadata.creationTimestamp>=$since)]|length==0' \
    "$out/workflows-hold-released.json" >/dev/null || { echo "a restored run was dispatched once admission opened" >&2; return 1; }
  rehearsal_runs hold-released
  jq -se '[.[]|select(.id)]|all(.status=="interrupted")' "$out/runs-hold-released.jsonl" >/dev/null ||
    { echo "a restored run became active again" >&2; return 1; }
  jq -se --slurpfile before <(jq -s 'map(select(.id))|map(.id)|sort' "$out/runs-before.jsonl") \
    'map(select(.id))|map(.id)|sort == $before[0]' "$out/runs-hold-released.jsonl" >/dev/null ||
    { echo "runs were created after admission opened" >&2; return 1; }
  k -n oberth get pods -l app.kubernetes.io/name=oberth -o json >"$out/pods-rolled-back.json"
  jq -e '.items|length==1 and all(.[]; .status.phase=="Running" and all(.status.containerStatuses[]; .ready==true))' "$out/pods-rolled-back.json" >/dev/null
  printf 'rehearsal: %s -> %s (schema %s -> %s) -> rollback to %s; run admitted after the upgrade: %s; holder %s\n' \
    "${candidate_versions[$a]}" "${candidate_versions[$b]}" "$schema_a" "$schema_b" "${candidate_versions[$a]}" "$target" "$holder" >"$out/summary.txt"
}

# rehearse_rollback_to_published: published release --oberth upgrade-->
# rehearsal build (one throwaway migration) --oberth upgrade --rollback-->
# published release. The published server does not read the restored-runs
# marker, so the restore must hold admission in the restored database: the
# backup's queued runs stay queued until the hold is released. The seed hold
# (five minutes) covers the pre-upgrade backup and then lapses, so the
# upgraded server claims that work, and the backup's own hold has lapsed by
# the time of the restore.
rehearse_rollback_to_published() {
  local b=rehearsal out="$evidence/rehearsal" home="$private/home"
  local cli="$private/oberth-$b" schema_a schema_b record holder hold_until
  mkdir -p "$out" "$home"
  schema_b=$("$private/oberth-$b" version --schema)
  rehearsal_version "v$from" before
  k -n oberth-argo wait --for=jsonpath='{.status.phase}'=Running workflow/oberth-e2e-running --timeout=60s
  rehearsal_runs before
  schema_a=$(jq -sr 'map(select(.schema))|.[0].schema' "$out/runs-before.jsonl")
  [[ $schema_b == $((schema_a+1)) ]] || { echo "rehearsal schema $schema_b is not $schema_a+1" >&2; return 1; }
  jq -se '([.[]|select(.status=="running")]|length)==1 and ([.[]|select(.status=="queued")]|length)==4' "$out/runs-before.jsonl" >/dev/null
  hold_until=$(( $(date +%s) + 300 ))

  awk -v ref="${candidate_refs[$b]}" '/^image:/{img=1} img && /^  ref:/{print "  ref: " ref; img=0; next} {print}' \
    "$private/source-$b/charts/oberth/values.yaml" >"$private/rehearsal-values.yaml"
  mv "$private/rehearsal-values.yaml" "$private/source-$b/charts/oberth/values.yaml"
  HOME=$home nice "$cli" upgrade --chart "$private/source-$b/charts/oberth" --yes --backup-dir "$out" \
    --set image.pullPolicy=Never --timeout 5m >"$out/upgrade.log" 2>&1 || { cat "$out/upgrade.log" >&2; return 1; }
  # The backup must have been taken while the seed hold still held the runs.
  (( $(date +%s) < hold_until )) || { echo "the seed hold lapsed before the upgrade finished; the backup may hold no queued runs" >&2; return 1; }
  rehearsal_version "${candidate_versions[$b]}" upgraded
  record=$(find "$out" -maxdepth 1 -name 'oberth-upgrade-*.json' -print -quit)
  jq -e --argjson a "$schema_a" --argjson b "$schema_b" '.schemaBefore==$a and .schemaAfter==$b and (.backup|length)>0' "$record" >/dev/null

  # Work on the new version once the seed hold lapses.
  holder=$(jq -sr '.[]|select(.status=="running")|.id' "$out/runs-before.jsonl")
  k -n oberth-argo delete workflow oberth-e2e-running --wait=true
  for ((i=0;i<120;i++)); do
    rehearsal_runs dispatched
    if jq -se '[.[]|select(.id)]|all(.status!="queued")' "$out/runs-dispatched.jsonl" >/dev/null; then break; fi
    sleep 5
  done
  jq -se '[.[]|select(.id)]|all(.status!="queued")' "$out/runs-dispatched.jsonl" >/dev/null ||
    { echo "the upgraded server did not claim the queued runs after the seed hold lapsed" >&2; return 1; }

  HOME=$home nice "$cli" upgrade --rollback "$record" --yes --timeout 5m >"$out/rollback.log" 2>&1 || { cat "$out/rollback.log" >&2; return 1; }
  rg -qF 'oberth admission hold off --reason "restored runs checked"' "$out/rollback.log" ||
    { echo "the rollback placed no admission hold" >&2; return 1; }
  rehearsal_version "v$from" rolled-back
  k -n oberth rollout status deploy/oberth --timeout=180s
  sleep 45
  # The published server runs the restored history, held: the backup's queued
  # runs are still queued (not dispatched again), and nothing was added.
  rehearsal_runs rolled-back
  jq -se --argjson s "$schema_a" '(map(select(.schema))|.[0].schema)==$s' "$out/runs-rolled-back.jsonl" >/dev/null
  jq -se --slurpfile before <(jq -s 'map(select(.id))|map(.id)|sort' "$out/runs-before.jsonl") \
    'map(select(.id))|map(.id)|sort == $before[0]' "$out/runs-rolled-back.jsonl" >/dev/null ||
    { echo "restored run set differs from the backup's" >&2; return 1; }
  jq -se --arg holder "$holder" '[.[]|select(.id and .id!=$holder)]|all(.status=="queued")' "$out/runs-rolled-back.jsonl" >/dev/null ||
    { echo "the published server dispatched restored queued runs despite the hold" >&2; return 1; }
  # Releasing the hold is the operator's decision; once released, the
  # published server dispatches that work (it does not read the marker).
  rehearsal_release_hold published "$fixture_image"
  for ((i=0;i<60;i++)); do
    rehearsal_runs released
    if jq -se --arg holder "$holder" '[.[]|select(.id and .id!=$holder)]|all(.status!="queued")' "$out/runs-released.jsonl" >/dev/null; then break; fi
    sleep 3
  done
  jq -se --arg holder "$holder" '[.[]|select(.id and .id!=$holder)]|all(.status!="queued")' "$out/runs-released.jsonl" >/dev/null ||
    { echo "the hold, not something else, should have been what held the runs" >&2; return 1; }
  printf 'rehearsal: %s -> %s (schema %s -> %s) -> rollback to %s; restored queued runs held until release; holder %s\n' \
    "v$from" "${candidate_versions[$b]}" "$schema_a" "$schema_b" "v$from" "$holder" >"$out/summary.txt"
}
