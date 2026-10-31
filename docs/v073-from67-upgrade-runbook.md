# tuxbox: v0.17.67/schema33 to v0.17.73/schema35 and back

For v0.17.74, follow the [schema36 upgrade note](schema36-upgrade-note.md)
for the **33→36→33** migration/restore assertions and new release pins.
This runbook's pinned 33→35→33 rehearsal remains historical evidence, not
qualification of schema36. Its zero-writer, old-image schema-33 restore
procedure remains the rollback basis; do not start the old binary on schema36.

Use the exact old/new pins described in [from-67 pin preparation](v073-from67-pins.md).
The v0.17.62 CLI currently on the operator machine must never perform this
backup/restore. Use absolute paths to the verified v0.17.67 and v0.17.73 CLIs;
the blocks check the old CLI file hash before executing it. Rehearsal evidence
is delivered as V073-FROM67-QUALIFICATION.md/log/json; see the
[qualification and its limits](v073-from67-qualification.md).

The code candidate stays at **863a88eb80244f0d7cc8ee96852e3e1e64381f89**.
This docs-only adaptation targets tuxbox's reported **v0.17.67/schema33**;
it does not move that code head or tag. Startup applies migration **34**
(queue_priority_patterns and run_priority_overrides) and **35**
(runs_latest_ref_idx), with their ledger entries, in one transaction.
v0.17.67 supports only schema33 and refuses either 34 or 35. Rollback requires
a verified, fresh pre-upgrade schema33 history backup, with the pinned old
binary/chart staged at zero replicas before replacement. No index/ledger edits
and no ordinary Helm rollback against the upgraded database.
`oberth restore` alone does not downgrade the binary: using the new CLI/current
new chart can restore old data and immediately migrate it back to 35.

## Before upgrading

Drain running KubeVirt/resource-delete work and wait for cleanup; drain other
active work where practical. Retain old image/chart pins, the complete old
values, compatibility identities and backup recipient private key off-volume.
Record separate OpenBao recovery state and live Workflow/VM ownership. A history
backup does not contain OpenBao secrets/policies/grants, values or log/artifact
files, and does not reverse external publishing or resource operations.

On a disposable install/copy, rehearse with the exact intended old/new chart,
image, CLI and installation identities first. The local rehearsal below covers
the command ordering and schema transition; it does not qualify production
keys, storage or external witness authorization. If continuity cannot be proved,
plan forward repair rather than promising a rollback.

These Bash blocks use an explicit kubeconfig/context; fill and verify every
path/pin. `OLD_VALUES` must be a protected export reviewed against the old chart,
with no new-only fields such as `admission.runtimeCapacity`. Do not use
`--reuse-values` from the new release to stage the old chart. Put OpenSSL 3,
Helm 4, kubectl, Python 3 and normal host utilities in PATH. Space is needed for
several database copies; use a private disk-backed working directory.

The variable-only block below is safe to run in a fresh emergency shell: it
does not capture evidence, back up or change the cluster. Reuse the original
WORK/BACKUP paths; do not re-run the pre-upgrade capture block during recovery.

```bash
set -euo pipefail
: "${KCFG:?explicit kubeconfig}" "${CTX:?target context}" "${NS:?namespace}"
: "${ARGO_NS:?Workflow namespace}" "${OLD_CLI:?pinned v0.17.67 executable}"
: "${OLD_CHART:?verified unpacked v0.17.67 chart}" "${OLD_VALUES:?old values file}"
: "${BACKUP:?fresh off-volume backup output path}" "${KEY:?recipient private key}"
: "${OLD_IMAGE:?exact old image ref including sha256 digest}"
: "${OLD_CLI_SHA256:?platform digest from verified v0.17.67 SHA256SUMS}"
: "${NEW_CLI:?verified v0.17.73 executable}" "${NEW_CLI_SHA256:?verified new CLI digest}"
: "${NEW_CHART:?git-tag chart stamped v0.17.73}" "${NEW_VALUES:?reviewed live values plus chosen overrides}"
: "${NEW_IMAGE:?approved exact new image ref including sha256 digest}"
: "${WORK:?private recovery evidence directory}"
umask 077
k=(kubectl --kubeconfig "$KCFG" --context "$CTX" -n "$NS")
h=(helm --kubeconfig "$KCFG" --kube-context "$CTX")
c=(--kubeconfig "$KCFG" --context "$CTX" --namespace "$NS" --argo-namespace "$ARGO_NS")
```

Capture pre-upgrade evidence once, into a new private directory. This block
refuses an existing directory so it cannot overwrite the original evidence.

```bash
: "${CERT:?recipient certificate}"
test ! -e "$WORK"
mkdir -m 700 "$WORK"
test "$(shasum -a 256 "$OLD_CLI" | awk '{print $1}')" = "$OLD_CLI_SHA256"
test "$("$OLD_CLI" version --schema)" = 33  # support, not the DB ledger
"$OLD_CLI" version
test "$("${k[@]}" exec deploy/oberth -c oberth -- /usr/local/bin/oberth version --schema)" = 33
"${h[@]}" get values oberth -n "$NS" --all > "$WORK/installed-values.yaml"
"${k[@]}" get deploy/oberth -o json > "$WORK/pre-upgrade-deployment.json"
"${k[@]}" get cm -l oberth.ci/audit-witness -o json > "$WORK/pre-upgrade-witnesses.json"
"${k[@]}" exec deploy/oberth -c oberth -- /usr/local/bin/oberth audit head > "$WORK/pre-upgrade-head.txt"
"$OLD_CLI" backup --history-only --recipient "$CERT" --output "$BACKUP" "${c[@]}"
"$OLD_CLI" restore "$BACKUP" --inspect --private-key "$KEY" | tee "$WORK/backup-inspect.txt"
shasum -a 256 "$BACKUP" > "$WORK/backup.sha256"
```

Require the verified archive's actual schema to be 33. Keep the archive/key in
separate protected off-volume locations. Take this backup immediately before
upgrade; an old archive or a schema-35 archive is not the downgrade backup.
`--inspect` decrypts and verifies hashes, integrity, foreign keys and audit chain
without touching a cluster. It is not a witness-continuity authorization check.

The v0.17.67/v3 online backup contains a single checkpointed database inside the
encrypted envelope. After successful CLI inspection, the following extracts
only fixed members into the private evidence directory. It records the exact
backup audit head and retains a read-only check copy for the witness screen.
Remove the decrypted copies when recovery evidence no longer needs them.

```bash
openssl cms -decrypt -binary -inform DER -in "$BACKUP" -inkey "$KEY" \
  -out "$WORK/verified-envelope.tar.gz"
tar -xOzf "$WORK/verified-envelope.tar.gz" manifest.json > "$WORK/backup-manifest.json"
tar -xOzf "$WORK/verified-envelope.tar.gz" database/oberth-database.tar > "$WORK/database.tar"
tar -xOf "$WORK/database.tar" oberth.sqlite > "$WORK/backup-check.sqlite"
rm "$WORK/verified-envelope.tar.gz" "$WORK/database.tar"
python3 - "$WORK/backup-manifest.json" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
assert m['format'] == 'oberth.backup/v3'
assert m['database']['schema'] == 33
print('Rollback backup:', m['database'])
PY
```

## Apply the new chart from the tag, with reviewed live values

Tuxbox uses Helm with a chart extracted from the Git tag and hand-stamped
Chart.yaml, not `oberth upgrade`. Current candidate source metadata says
0.17.70/v0.17.70; the actual v0.17.67 tag says 0.17.5/v0.17.5. Neither is a
usable release version pin until stamped. Set version/appVersion to 0.17.67/
v0.17.67 for the old chart and 0.17.73/v0.17.73 for the new chart, and set the
exact corresponding image digest. Never deploy the raw in-tree bootstrap image.
Verify the eventual v0.17.73 tag resolves to the approved code head. The local
rehearsal uses that exact head, not a made-up published v0.17.73 tag or image.

Use a full reviewed live-values export, preserving compatibility identities,
controller profile, namespaces, storage and trusted execution settings. Apply
only explicitly chosen capacity/PSI changes. The reported live maxConcurrentJobs
is 4; the proposed first-hours policy preserves 4, IO full threshold30,
hysteresis10 (resume below20), CPU threshold0 and maxDefer20m. Confirm the
coordinator's choice before production; this doc does not change live values.
During a long IO hold LOWER capacity; do not raise it. Keep /livez enabled.
[Watch items and the pre-73 change digest](v073-from67-watch.md) explain why.

```bash
test "$(shasum -a 256 "$NEW_CLI" | awk '{print $1}')" = "$NEW_CLI_SHA256"
test "$("$NEW_CLI" version --schema)" = 35
"${h[@]}" upgrade oberth "$NEW_CHART" -n "$NS" --reset-values -f "$NEW_VALUES" \
  --set-string "image.ref=$NEW_IMAGE" --dry-run=server > "$WORK/upgrade-plan.txt"
# Review the rendered image, compatibility identities, PVC, roles and probes.
"${h[@]}" upgrade oberth "$NEW_CHART" -n "$NS" --reset-values -f "$NEW_VALUES" \
  --set-string "image.ref=$NEW_IMAGE"
"${k[@]}" rollout status deploy/oberth --timeout=180s
"${k[@]}" get deploy/oberth -o json > "$WORK/new-deployment.json"
python3 - "$WORK/new-deployment.json" "$NEW_IMAGE" <<'PYIMAGE'
import json, sys
d = json.load(open(sys.argv[1]))
image = next(c['image'] for c in d['spec']['template']['spec']['containers'] if c['name'] == 'oberth')
assert image == sys.argv[2], 'New image mismatch: stop'
PYIMAGE
test "$("${k[@]}" exec deploy/oberth -c oberth -- /usr/local/bin/oberth version --schema)" = 35
"$NEW_CLI" backup --history-only --recipient "$CERT" \
  --output "$WORK/post-upgrade-35.oberth-backup" "${c[@]}"
"$NEW_CLI" restore "$WORK/post-upgrade-35.oberth-backup" --inspect --private-key "$KEY" \
  | tee "$WORK/post-upgrade-inspect.txt"
```

The verified backup, not version --schema alone, proves the actual database is
35. The schema33 rollback archive must remain unchanged/off-volume. The new
binary would immediately migrate a restored old DB back to 35; use the OLD_CLI
and OLD_CHART in the emergency procedure below. The generic `oberth upgrade`
path and its flags are not part of the tuxbox procedure.

## The witness deadline

Check the live Deployment's `--audit-rekor-url=` argument, its Helm
`auditAnchor.rekorURL` value and the audit mode/status. An absent argument in an
unavailable/incomplete observation is not proof of local-only mode. Retain
ConfigMaps labelled `oberth.ci/audit-witness=continuity` and `=intent`; their
`data["record.json"]` records include auditID/auditSHA256, and receipts include
UUID/logIndex. Compare them to the verified backup's **actual prefix**, not to
the binary version or a separately sampled current head.

**The first Rekor publication of a head outside that backup's audit prefix can
close ordinary restore.** Often this is the first publication after upgrade;
it can happen earlier because backup itself appends `backup.completed` after
taking its snapshot. The default ten-minute checkpoint interval is not a
rollback guarantee. An unresolved publication intent may already represent an
external write whose acknowledgement was lost; treat it as uncertain and stop.
TSA checkpoints are not a substitute for checking the Rekor/continuity policy.

With the server stopped, collect the records again and run this necessary
screen. It checks the real saved prefix and refuses pending intents. This does
**not** verify Rekor receipt signatures, prove the remote history is complete,
or override continuity checks. Where Rekor is enabled, require the witness
operator's authenticated current history/receipt verification and reconciliation
of pending intents as well. If remote history is unavailable or its relationship
to the backup cannot be proved, stop and choose forward repair. Do not delete
ConfigMaps, reset witnesses, disable Rekor, or set witness-genesis/reset flags
to make an old backup acceptable. An empty local list alone is insufficient.

```bash
"${k[@]}" get cm -l oberth.ci/audit-witness -o json > "$WORK/current-witnesses.json"
python3 - "$WORK/backup-check.sqlite" "$WORK/current-witnesses.json" <<'PY'
import json, pathlib, sqlite3, sys
uri = pathlib.Path(sys.argv[1]).resolve().as_uri() + '?mode=ro&immutable=1'
with sqlite3.connect(uri, uri=True) as db:
    assert db.execute('select max(version) from schema_migrations').fetchone()[0] == 33
    for cm in json.load(open(sys.argv[2]))['items']:
        kind = cm['metadata']['labels']['oberth.ci/audit-witness']
        if kind not in ('continuity', 'intent'):
            raise SystemExit('Unknown witness evidence: stop')
        r = json.loads(cm['data']['record.json'])
        row = db.execute('select sha256 from audit_actions where id=?', (r['auditID'],)).fetchone()
        if row is None or row[0].hex() != r['auditSHA256'].lower():
            raise SystemExit('Witness head outside rollback backup: use forward repair')
        if kind == 'intent':
            raise SystemExit('Pending witness intent: reconcile and re-check before restore')
print('Local prefix screen passed; external witness verification is still required when configured')
PY
```

## Named emergency procedure: old chart staged at zero, old CLI history restore

Pause GitOps, HPA and any other actor that can restart writers or change the
Deployment. Stop new submissions. Preserve the current schema-35 state: take a
verified encrypted backup with the new CLI, retain logs/values/audit/witness
records and, after stopping, preserve the stopped PVC/DB/WAL/SHM using the
installation's storage recovery mechanism. Keep sufficient volume headroom for
restore staging and `/data/pre-restore-<timestamp>/`. Do not delete these files.

The old chart fixes `replicaCount` at one in **both** its values schema and a
Helm template validation. `helm upgrade ... --set replicaCount=0` fails; skipping
schema validation does not bypass the template check. Instead make a reviewed,
throwaway maintenance copy of the pinned old chart with exactly one change:
its Deployment renders `replicas: 0`, while values still say `replicaCount: 1`.
The normal old chart remains untouched. This avoids starting an old Pod against
schema 35. No CLI/server/schema code changes are involved.

```bash
"${k[@]}" scale deploy/oberth --replicas=0
"${k[@]}" wait --for=delete pod -l 'app.kubernetes.io/name=oberth,app.kubernetes.io/instance=oberth' --timeout=180s
MAINT_CHART="$WORK/old-chart-maintenance"
test ! -e "$MAINT_CHART"
cp -R "$OLD_CHART" "$MAINT_CHART"
python3 - "$MAINT_CHART/templates/deployment.yaml" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1])
s = p.read_text()
needle = '  replicas: {{ .Values.replicaCount }}'
assert s.count(needle) == 1, 'Unexpected pinned chart: stop and review, do not improvise'
p.write_text(s.replace(needle, '  replicas: 0'))
print('Sole maintenance edit: Deployment replicas template -> literal 0')
PY
"${h[@]}" template oberth "$MAINT_CHART" -n "$NS" -f "$OLD_VALUES" > "$WORK/old-maintenance-render.yaml"
# Review the render: Deployment replicas 0; exact old image; original PVC and identities.
"${h[@]}" upgrade oberth "$MAINT_CHART" -n "$NS" --reset-values -f "$OLD_VALUES"
"${k[@]}" get deploy/oberth -o json > "$WORK/staged-old-deployment.json"
"${k[@]}" get pods -l 'app.kubernetes.io/name=oberth,app.kubernetes.io/instance=oberth' -o json > "$WORK/staged-old-pods.json"
python3 - "$WORK/staged-old-deployment.json" "$WORK/staged-old-pods.json" "$OLD_IMAGE" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
assert d['spec']['replicas'] == 0
assert not json.load(open(sys.argv[2]))['items'], 'Server Pod exists: stop'
image = next(c['image'] for c in d['spec']['template']['spec']['containers'] if c['name'] == 'oberth')
assert image == sys.argv[3], f'Staged image mismatch: {image} != {sys.argv[3]}; stop'
print('Staged old image verified:', image)
PY
# Re-check current witness records/prefix and authoritative remote history now (above).
test "$(shasum -a 256 "$OLD_CLI" | awk '{print $1}')" = "$OLD_CLI_SHA256"
"$OLD_CLI" restore "$BACKUP" --only history --private-key "$KEY" --chart "$OLD_CHART" \
  "${c[@]}" --timeout=180s --dry-run
# Review the concrete plan before replacing history.
"$OLD_CLI" restore "$BACKUP" --only history --private-key "$KEY" --chart "$OLD_CHART" \
  "${c[@]}" --timeout=180s --yes
"${k[@]}" rollout status deploy/oberth --timeout=180s
"${k[@]}" exec deploy/oberth -c oberth -- /usr/local/bin/oberth version --schema
"${h[@]}" upgrade oberth "$OLD_CHART" -n "$NS" --reset-values -f "$OLD_VALUES"
"${k[@]}" rollout status deploy/oberth --timeout=180s

"$OLD_CLI" backup --history-only --recipient "$CERT" --output "$WORK/final-33.oberth-backup" "${c[@]}"
"$OLD_CLI" restore "$WORK/final-33.oberth-backup" --inspect --private-key "$KEY"
```

The installed utility image must be the installation's qualified digest-pinned
file-tool image, accessible on the node; `restore --utility-image PIN` can
explicitly select it. The server image is FROM scratch, so do not try to execute
`sh`/`sqlite3` there. The restore helper uses the stopped PVC, verifies upload
hash/size/ownership, keeps the original files in `/data/pre-restore-<timestamp>/`,
and uses the staged old server image's `version --schema` before the swap.
Restore itself starts `max(current replicas, 1)`; from zero it starts one old
Pod. Its successful exit alone does not prove readiness: explicitly check the
rollout, actual image, health and logs. Take a fresh old-CLI backup and inspect
it to verify actual schema 33; `version --schema` only reports binary support.

History restore loses every post-backup DB change, including uplinks/tokens and
audit entries. It deletes Argo Workflows whose run IDs are absent from the
restored DB; capture their state first, drain them beforehand and reconcile
external resources separately. The rehearsal verified an actual orphan deletion.
`--only history` preserves OpenBao KV/policies/roles/grants and install identities.
A full restore can rewrite secrets and is not the emergency downgrade default.

If a command fails, leave the Deployment at zero and read its explicit
pre-swap/post-swap error. Once the DB is swapped, scaling the new image would
migrate it again. Do not blindly follow a generic scale-up suggestion without
checking which image and database are in place. Preserve recovery evidence and
use forward repair if continuity or the old deployment cannot be established.
