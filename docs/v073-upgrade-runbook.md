# v0.17.73: schema-35 upgrade and emergency history restore

For tuxbox's actual v0.17.67/schema33 baseline, use the
[from-67 runbook](v073-from67-upgrade-runbook.md), including verified v0.17.67
CLI/chart pins and schema33 asserts. The v0.17.62 operator CLI is not valid.
This original 72-to-73 procedure remains for genuine schema34 installations.

For v0.17.74/schema 36, see [the separate upgrade/rollback qualification note](schema36-upgrade-note.md). The pins and assertions here remain specific to v0.17.73.

Use the coordinator's final candidate pins. This procedure does not deploy a
candidate or authorize changing a production installation. Rehearsal evidence
and its limits are in [schema-35 qualification](schema35-rollback-qualification.md).

Schema 35 adds `runs_latest_ref_idx`. v0.17.72 supports schema 34 and refuses
schema 35. Rollback is **restore of a verified pre-upgrade schema-34 backup**,
with the old binary/chart staged before restarting. No index or migration-ledger
edits and no ordinary `helm rollback` against the upgraded database.
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
: "${ARGO_NS:?Workflow namespace}" "${OLD_CLI:?pinned v0.17.72 executable}"
: "${OLD_CHART:?verified unpacked v0.17.72 chart}" "${OLD_VALUES:?old values file}"
: "${BACKUP:?fresh off-volume backup output path}" "${KEY:?recipient private key}"
: "${OLD_IMAGE:?exact old image ref including sha256 digest}"
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
"$OLD_CLI" version --schema  # expected 34: binary support, not the DB ledger
"${k[@]}" exec deploy/oberth -c oberth -- /usr/local/bin/oberth version --schema
"${h[@]}" get values oberth -n "$NS" --all > "$WORK/installed-values.yaml"
"${k[@]}" get deploy/oberth -o json > "$WORK/pre-upgrade-deployment.json"
"${k[@]}" get cm -l oberth.ci/audit-witness -o json > "$WORK/pre-upgrade-witnesses.json"
"${k[@]}" exec deploy/oberth -c oberth -- /usr/local/bin/oberth audit head > "$WORK/pre-upgrade-head.txt"
"$OLD_CLI" backup --history-only --recipient "$CERT" --output "$BACKUP" "${c[@]}"
"$OLD_CLI" restore "$BACKUP" --inspect --private-key "$KEY" | tee "$WORK/backup-inspect.txt"
shasum -a 256 "$BACKUP" > "$WORK/backup.sha256"
```

Require the verified archive's actual schema to be 34. Keep the archive/key in
separate protected off-volume locations. Take this backup immediately before
upgrade; an old archive or a schema-35 archive is not the downgrade backup.
`--inspect` decrypts and verifies hashes, integrity, foreign keys and audit chain
without touching a cluster. It is not a witness-continuity authorization check.

The v0.17.72/v3 online backup contains a single checkpointed database inside the
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
assert m['database']['schema'] == 34
print('Rollback backup:', m['database'])
PY
```

Only then apply the coordinator's pinned upgrade. On tuxbox, the coordinator
reports `audit_mode=local` (no Rekor), and upgrades with Helm using the chart
from the Git tag, **not** `oberth upgrade`. The rehearsal uses the same Helm
upgrade path; re-check the actual audit mode/configuration before each operation.
The generic `oberth upgrade` command has no `--atomic` or `--force-replace`;
if using that separate installer path, use `--dry-run`, review, then `--yes`.
Confirm the new binary's support and a newly verified backup's actual schema 35,
readiness/liveness, admission and run recovery. During a long IO-pressure hold,
lower admission capacity; do not raise it. Existing grants drain without being
cancelled; raising capacity can worsen contention as pressure clocks expire.
New writes and witness publication
can make the old backup unusable even when schema compatibility was rehearsed.

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
    assert db.execute('select max(version) from schema_migrations').fetchone()[0] == 34
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
"$OLD_CLI" restore "$BACKUP" --only history --private-key "$KEY" --chart "$OLD_CHART" \
  "${c[@]}" --timeout=180s --dry-run
# Review the concrete plan before replacing history.
"$OLD_CLI" restore "$BACKUP" --only history --private-key "$KEY" --chart "$OLD_CHART" \
  "${c[@]}" --timeout=180s --yes
"${k[@]}" rollout status deploy/oberth --timeout=180s
"${k[@]}" exec deploy/oberth -c oberth -- /usr/local/bin/oberth version --schema
"${h[@]}" upgrade oberth "$OLD_CHART" -n "$NS" --reset-values -f "$OLD_VALUES"
"${k[@]}" rollout status deploy/oberth --timeout=180s
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
it to verify actual schema 34; `version --schema` only reports binary support.

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
