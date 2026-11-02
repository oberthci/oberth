# Schema 37: container telemetry and rollback

> Historical release evidence. These pinned procedures do not describe the current release.
> For current operations, use [backup/restore](../backup-restore.md) and the shipped CLI help.

Schema 37 retains the version-2 observation-format guard. It also adds
`step_resource_usage_retention_idx(last_observed_at)` for oldest-first bounded
cleanup. Schema-36 table/index definitions and version-1 rows are preserved;
new observations upgrade an existing main incarnation in place. Unlike the
initial measurement patch's no-op guard, this migration builds an index over
existing usage rows. Qualify that one-time build on the real database size.
The migration applies even with `argo.resourceSampler.enabled: false`.

**Helm rollback alone stops the server with schema-too-new.** A schema-36 binary
refuses schema 37, and the chart's Recreate strategy leaves no old Pod serving.
Rollback requires restoring the independently verified pre-upgrade database
backup for the selected old image/schema, following the existing zero-writer
maintenance-chart, identity, OpenBao and audit-continuity restore procedure.
Restoring that backup loses database writes made after the backup. Never edit
the migration ledger or drop the new index to simulate a database downgrade.

The [schema-36 upgrade note](schema36-upgrade-note.md) remains historical for
that target. For a schema-36 predecessor, qualification must pin the actual old
and new chart/image/CLI digests and rehearse **36 -> 37 -> restored 36**:

1. Pause intake and drain all Workflow/VM/helper work. Independently inspect the
   encrypted pre-upgrade backup, including schema, integrity, foreign keys,
   audit head/domain and original immutable identities. Retain the old pins.
2. Upgrade the pinned new chart/image. Verify schema 37, the retention index,
   unchanged old ledger entries/audit prefix, readable version-1 rows, valid
   version-2 observations and application/OpenBao/token-delivery health.
3. Confirm the pinned old binary refuses the upgraded database as schema-too-new
   in an isolated rehearsal, with the disposable target's writers stopped before
   any restore.
4. Restore the verified schema-36 backup using the pinned old CLI and zero-writer
   maintenance procedure. Verify exact restored schema/integrity/FKs/audit
   prefix/restore receipt, original identities, one Ready old server, VM cleanup
   and working OpenBao authentication/token delivery before resuming intake.

A schema-33 or schema-35 predecessor instead needs its own matching backup and
pinned **33 -> 37 -> restored 33** or **35 -> 37 -> restored 35** rehearsal. The
historical schema-35/schema-36 evidence does not qualify these paths.

The source tests cover migration preservation and the existing schema-too-new
refusal boundary. No live Helm rollback or encrypted operator restore rehearsal
was performed in this change; those remain release-qualification work.

## Coordinator rehearsal on tuxbox: 36 -> 37 -> restored 36

Run this before tagging on a **fresh disposable schema-36 installation only**
on tuxbox. Never run this rehearsal against the actual production installation,
including during maintenance. It needs three dedicated namespaces: Oberth (`NS`),
Argo (`ARGO_NS`) and OpenBao (`BAO_NS`), its own PVC and immutable identities,
and synthetic history created on that fresh install. Do not restore production
history or reuse production values, witnesses, credentials or backup keys.
The blocks assume the verified disposable release and Deployment are named `oberth`.

Install the rehearsal Argo controller with `crds.install=false`, because CRDs are cluster-wide and shared with production.

Provision an Argo controller watching only the dedicated rehearsal namespace,
and authenticate with the disposable OpenBao's credentials. Keep Rekor unset;
no rehearsal head should be published to the production or public witness log.
Choose ClusterIP services or rehearsal-only NodePorts that do not collide with
production's ports (defaults 30022/30443). Budget the extra server/OpenBao and
private copies against tuxbox's host/disk limits before provisioning. A fresh
synthetic fixture does not qualify production-size index-build latency.

Record the verified production namespace names separately. Label each newly
provisioned disposable namespace `oberth.ci/rehearsal-id=$REHEARSAL_ID` during
its creation. The mandatory guard below refuses any production-name match,
requires three distinct disposable namespaces, and checks that all three carry
that marker in the explicit context. Never add the marker to an existing
production namespace to pass the check.

Use native tuxbox executables and digest-pinned old/new images and charts from
approved source/release artifacts. The old executable must actually support
schema 36, the new one 37. Historical schema-33/35 pins do not meet this test.
Keep normal old/new charts untouched after verification; the maintenance copy
below has one reviewed replica-template edit. No step uses `oberth upgrade` or
`--reuse-values`. Do not put keys/tokens in values, commands or evidence logs.

### Backup private-key access: rehearsal-only pair

The initial block runs `oberth backup keygen --dir "$KEYDIR"` in a new private
directory and derives `CERT` and `KEY` from that generated pair. Do not supply a
production key, `~/.config/oberth/backup/recipient.key`, or a production backup.
The directory guard rejects the normal backup-key directory. No command prints
private key bytes. History-only backup/restore requires no OpenBao root token;
any separate OpenBao health credential belongs only to the disposable install.

| Step | Backup private-key access |
| --- | --- |
| Pin/namespace checks, drain, Helm, scale, archive hashes | None |
| `backup keygen --dir "$KEYDIR"` | Creates the throwaway pair; no production key read |
| Three `backup --history-only --recipient "$CERT"` calls | Public certificate only |
| `inspect_history before36` | **KEY USE 1-2:** `restore --inspect` and `openssl cms -decrypt` each read `KEY` |
| `inspect_history after37` | **KEY USE 3-4:** the same two private-key reads |
| Offline old-binary refusal; witness-prefix screen | No new key read; consume plaintext from those inspections |
| `restore --dry-run` | **KEY USE 5:** decrypts with `KEY` |
| `restore --yes` | **KEY USE 6:** decrypts with `KEY` |
| `inspect_history restored36` | **KEY USE 7-8:** the same two private-key reads |
| Final ledger/audit comparison | No new key read; consumes inspected temporary databases |

All eight private-key reads are essential and use only the throwaway pair.
If that pair is unavailable, stop; do not fall back to the normal production key.
Production key/data use is outside this procedure.

### 1. Pin the target and preserve a schema-36 rollback point

Fill all variables from verified installation/release evidence, not guesses.
`OLD_VALUES` and `NEW_VALUES` are complete reviewed exports valid for their
respective disposable chart; preserve its own PVC, namespaces, public protocol
identities and utility-image pins, with conservative rehearsal capacity. Neither
file may be an export of production values. `WORK` is a new private evidence
directory; decrypted archives/databases go only into a separate temporary
`PLAIN` directory and are removed by an EXIT trap, including failure paths.
`PLAIN_ROOT` must have room for several database copies plus staging reserve.
`KEYDIR` must be a new rehearsal-only private directory outside `WORK`, `PLAIN`
and the normal production backup-key directory. Keep encrypted archives and
this throwaway key pair separately; never archive the key into `WORK`.
Use Bash, Helm, kubectl, OpenSSL 3,
Python 3 and the verified CLIs.

```bash
set -euo pipefail
umask 077
: "${KCFG:?explicit kubeconfig}" "${CTX:?tuxbox rehearsal context}"
: "${NS:?fresh rehearsal Oberth namespace}" "${ARGO_NS:?dedicated rehearsal Argo namespace}"
: "${BAO_NS:?dedicated rehearsal OpenBao namespace}" "${REHEARSAL_ID:?fresh disposable namespace marker}"
: "${PROD_NS:?verified production Oberth namespace}"
: "${PROD_ARGO_NS:?verified production Argo namespace}"
: "${PROD_BAO_NS:?verified production OpenBao namespace}"
: "${OLD_CLI:?absolute native schema-36 CLI}" "${NEW_CLI:?absolute native schema-37 CLI}"
: "${OLD_CLI_SHA256:?verified old CLI hash}" "${NEW_CLI_SHA256:?verified new CLI hash}"
: "${OLD_IMAGE:?old image including sha256 digest}" "${NEW_IMAGE:?new image including sha256 digest}"
: "${OLD_CHART:?verified normal old chart}" "${NEW_CHART:?verified new chart}"
: "${OLD_VALUES:?protected complete old values}" "${NEW_VALUES:?protected complete new values}"
: "${WORK:?new private evidence directory}" "${BACKUP36:?new off-volume backup filename}"
: "${KEYDIR:?new throwaway key-pair directory}" "${PLAIN_ROOT:?private temporary-copy parent}"
: "${MAX_DB_BYTES:?approved logical DB bound, at most 4294967296}"
: "${OBERTH_SCHEMA_IDENTITY:?original public schema identity}"
: "${OBERTH_AUDIT_DOMAIN:?original public audit domain}"
: "${OBERTH_WITNESS_KEY_INFO:?original public witness key info}"
export OBERTH_SCHEMA_IDENTITY OBERTH_AUDIT_DOMAIN OBERTH_WITNESS_KEY_INFO
readonly KCFG CTX NS ARGO_NS BAO_NS REHEARSAL_ID PROD_NS PROD_ARGO_NS PROD_BAO_NS
guard_rehearsal() {
  python3 - "$NS" "$ARGO_NS" "$BAO_NS" "$PROD_NS" "$PROD_ARGO_NS" "$PROD_BAO_NS" <<'PY' || return 1
import sys
rehearsal = sys.argv[1:4]; production = set(sys.argv[4:7])
if not (all(rehearsal) and all(production)): raise SystemExit('Namespace inventory is required')
if len(set(rehearsal)) != 3: raise SystemExit('Oberth, Argo and OpenBao need separate namespaces')
if set(rehearsal) & (production | {'default', 'kube-system', 'kube-public', 'kube-node-lease'}):
    raise SystemExit('Production/reserved namespace refused')
PY
  for namespace in "$NS" "$ARGO_NS" "$BAO_NS"; do
    test "$(kubectl --kubeconfig "$KCFG" --context "$CTX" get namespace "$namespace" \
      -o jsonpath='{.metadata.labels.oberth\.ci/rehearsal-id}')" = "$REHEARSAL_ID" || {
      echo 'Missing disposable namespace marker: stop' >&2; exit 1;
    }
  done
}
guard_rehearsal  # Required before backup, drain, Helm, scale or restore.
k=(kubectl --kubeconfig "$KCFG" --context "$CTX" -n "$NS")
h=(helm --kubeconfig "$KCFG" --kube-context "$CTX")
c=(--kubeconfig "$KCFG" --context "$CTX" --namespace "$NS" --argo-namespace "$ARGO_NS" --openbao-namespace "$BAO_NS")
python3 - "$WORK" "$KEYDIR" "$PLAIN_ROOT" <<'PY'
import pathlib, sys
work, keydir, plainroot = (pathlib.Path(p).resolve() for p in sys.argv[1:])
production_keys = (pathlib.Path.home()/'.config/oberth/backup').resolve()
if keydir.exists(): raise SystemExit('Use a new throwaway key directory')
if keydir == production_keys or production_keys in keydir.parents:
    raise SystemExit('Production backup-key directory refused')
if work == keydir or work in keydir.parents or keydir in work.parents:
    raise SystemExit('Key directory must be separate from WORK')
if work == plainroot or work in plainroot.parents: raise SystemExit('Plaintext must be outside WORK')
if not plainroot.is_dir(): raise SystemExit('Prepare the protected temporary-copy parent first')
PY
test ! -e "$WORK"; mkdir -m 700 "$WORK"
PLAIN=$(mktemp -d "$PLAIN_ROOT/oberth-1216-plain.XXXXXX")
chmod 700 "$PLAIN"
readonly PLAIN
cleanup_plaintext() { rm -rf -- "$PLAIN"; }
trap cleanup_plaintext EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
test ! -e "$BACKUP36"
test "$(sha256sum "$OLD_CLI" | cut -d' ' -f1)" = "$OLD_CLI_SHA256"
test "$(sha256sum "$NEW_CLI" | cut -d' ' -f1)" = "$NEW_CLI_SHA256"
test "$("$OLD_CLI" version --schema)" = 36
test "$("$NEW_CLI" version --schema)" = 37
# Create only this disposable install's throwaway pair. Do not supply CERT/KEY.
"$OLD_CLI" backup keygen --dir "$KEYDIR"
CERT="$KEYDIR/recipient.pem"
KEY="$KEYDIR/recipient.key"
readonly CERT KEY
"${k[@]}" cluster-info > "$WORK/target-cluster.txt"
"${h[@]}" get values oberth -n "$NS" --all -o json > "$WORK/installed-values.json"
"${k[@]}" get deploy/oberth -o json > "$WORK/before-deployment.json"
python3 - "$WORK/before-deployment.json" "$OLD_IMAGE" "$ARGO_NS" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
assert next(c['image'] for c in d['spec']['template']['spec']['containers']
            if c['name'] == 'oberth') == sys.argv[2]
args = next(c.get('args', []) for c in d['spec']['template']['spec']['containers'] if c['name'] == 'oberth')
assert '--argo-namespace='+sys.argv[3] in args, 'Wrong disposable Argo namespace'
assert all(a == '--audit-rekor-url=' for a in args if a.startswith('--audit-rekor-url=')), 'Rekor must be unset on the fresh rehearsal install'
PY
```

Pause only disposable intake and suspend only its GitOps/HPA/other writers.
Never drain or scale production. Drain the dedicated namespaces' Workflows, source/
artifact helpers, VMIs, launchers and cleanup, using actual API inventory as well
as run status. Confirm authenticated unsealed health/token delivery with the
disposable OpenBao's credentials; do not use production credentials.
Record current audit mode, Rekor configuration and witness
records. Keep intake paused throughout the rehearsal.

```bash
guard_rehearsal
"${k[@]}" get cm -l oberth.ci/audit-witness -o json > "$WORK/before-witnesses.json"
"$OLD_CLI" backup --history-only --recipient "$CERT" --output "$BACKUP36" \
  --max-database-bytes "$MAX_DB_BYTES" "${c[@]}"
sha256sum "$BACKUP36" > "$WORK/backup36.sha256"
```

Define this inspection helper once. It first invokes the matching CLI's offline
cryptographic/member/integrity/FK/audit verification, then extracts fixed members
from that verified archive into `PLAIN`, outside `WORK`. It proves the actual
ledger and index, rather than treating `version --schema` as the DB version.
Plaintext check copies contain disposable-install secrets. They are temporary
inputs, never archived as evidence, and the EXIT trap removes their entire
directory, including SQLite WAL/SHM files and decrypted envelope/tar members.

```bash
inspect_history() {
  local cli="$1" archive="$2" expected="$3" label="$4" p="$WORK/$4" q="$PLAIN/$4"
  "$cli" restore "$archive" --inspect --private-key "$KEY" \
    --max-database-bytes "$MAX_DB_BYTES" | tee "$p.inspect.txt"
  openssl cms -decrypt -binary -inform DER -in "$archive" -inkey "$KEY" \
    -out "$q.envelope.tar.gz"
  tar -xOzf "$q.envelope.tar.gz" manifest.json > "$p.manifest.json"
  tar -xOzf "$q.envelope.tar.gz" database/oberth-database.tar > "$q.database.tar"
  tar -xOf "$q.database.tar" oberth.sqlite > "$q.sqlite"
  python3 - "$p.manifest.json" "$q.sqlite" "$expected" <<'PY'
import json, os, pathlib, sqlite3, sys
m = json.load(open(sys.argv[1])); expected = int(sys.argv[3])
assert m['format'] == 'oberth.backup/v3'
assert m['database']['schema'] == expected
for key, env in [('schema_identity', 'OBERTH_SCHEMA_IDENTITY'),
                 ('audit_domain', 'OBERTH_AUDIT_DOMAIN'),
                 ('witness_key_info', 'OBERTH_WITNESS_KEY_INFO')]:
    assert m['compatibility'][key] == os.environ[env]
with sqlite3.connect(pathlib.Path(sys.argv[2]).resolve().as_uri()+'?mode=ro', uri=True) as db:
    assert db.execute('pragma integrity_check').fetchall() == [('ok',)]
    assert not db.execute('pragma foreign_key_check').fetchall()
    assert db.execute('select max(version) from schema_migrations').fetchone()[0] == expected
    exists = db.execute("select 1 from sqlite_schema where type='index' and name='step_resource_usage_retention_idx'").fetchone()
    assert bool(exists) == (expected == 37)
print('Verified actual schema', expected, 'with expected retention-index state')
PY
}
# KEY USE 1-2: restore --inspect and openssl cms -decrypt read the throwaway KEY.
inspect_history "$OLD_CLI" "$BACKUP36" 36 before36
```

Stop here if actual schema is not 36. Use the actual predecessor's separate
procedure rather than changing the assertions. Independently retain/inspect the
backup and confirm private-key custody before migration.

### 2. Upgrade, inspect actual schema 37 and prove old-binary refusal offline

Render/server-dry-run the exact new chart against the reviewed values and image;
review image, PVC, original identities, Recreate strategy, probes and sampler
setting. Keep `argo.resourceSampler.enabled=false` for the migration/restore test;
enabling collection needs a separate direct-kubelet qualification.

```bash
guard_rehearsal
"${h[@]}" upgrade oberth "$NEW_CHART" -n "$NS" --reset-values -f "$NEW_VALUES" \
  --set-string "image.ref=$NEW_IMAGE" --set argo.resourceSampler.enabled=false \
  --set-string "argo.namespace=$ARGO_NS" --set-string auditAnchor.rekorURL= \
  --dry-run=server > "$WORK/upgrade-plan.txt"
# Review the rendered plan before executing the next command.
"${h[@]}" upgrade oberth "$NEW_CHART" -n "$NS" --reset-values -f "$NEW_VALUES" \
  --set-string "image.ref=$NEW_IMAGE" --set argo.resourceSampler.enabled=false \
  --set-string "argo.namespace=$ARGO_NS" --set-string auditAnchor.rekorURL=
"${k[@]}" rollout status deploy/oberth --timeout=180s
"${k[@]}" get deploy/oberth -o json > "$WORK/after37-deployment.json"
python3 - "$WORK/after37-deployment.json" "$NEW_IMAGE" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
assert next(c['image'] for c in d['spec']['template']['spec']['containers']
            if c['name'] == 'oberth') == sys.argv[2]
PY
"$NEW_CLI" backup --history-only --recipient "$CERT" --output "$WORK/after37.backup" \
  --max-database-bytes "$MAX_DB_BYTES" "${c[@]}"
# KEY USE 3-4: restore --inspect and openssl cms -decrypt read the throwaway KEY.
inspect_history "$NEW_CLI" "$WORK/after37.backup" 37 after37
if "$OLD_CLI" backup database --database "$PLAIN/after37.sqlite" \
     --max-database-bytes "$MAX_DB_BYTES" > "$PLAIN/old-refusal.tar" 2> "$WORK/old-refusal.txt"; then
  echo 'Old binary unexpectedly accepted schema 37: stop' >&2; exit 1
fi
python3 - "$WORK/old-refusal.txt" <<'PY'
import pathlib, sys
assert 'database=37 binary=36' in pathlib.Path(sys.argv[1]).read_text()
PY
```

The negative check uses the extracted private schema-37 copy; it does not launch
an old server against the live PVC. Record startup/index-build elapsed time and
verify application/OpenBao/token-delivery health. Preserve post-upgrade history,
logs, current values, witnesses and stopped PVC state before rollback.

### 3. Stage the old image at zero replicas, recheck continuity, then restore

Preserve the verified schema-36 backup unchanged. Keep external writers suspended.
Scale down and wait for no server Pods. The old chart requires replicaCount=1,
so a reviewed maintenance copy renders a literal zero without changing normal
old chart values/schema; do not try ordinary Helm rollback against schema 37.

```bash
guard_rehearsal
"${k[@]}" scale deploy/oberth --replicas=0
"${k[@]}" wait --for=delete pod \
  -l 'app.kubernetes.io/name=oberth,app.kubernetes.io/instance=oberth' --timeout=180s
MAINT_CHART="$WORK/old-chart-maintenance"
test ! -e "$MAINT_CHART"; cp -R "$OLD_CHART" "$MAINT_CHART"
python3 - "$MAINT_CHART/templates/deployment.yaml" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1]); text = p.read_text()
needle = '  replicas: {{ .Values.replicaCount }}'
assert text.count(needle) == 1, 'Unexpected pinned chart; stop'
p.write_text(text.replace(needle, '  replicas: 0'))
PY
"${h[@]}" template oberth "$MAINT_CHART" -n "$NS" -f "$OLD_VALUES" \
  --set-string "image.ref=$OLD_IMAGE" --set-string "argo.namespace=$ARGO_NS" \
  --set-string auditAnchor.rekorURL= > "$WORK/old-maintenance-render.yaml"
# Review: zero server replicas, exact old image, original PVC/identities.
"${h[@]}" upgrade oberth "$MAINT_CHART" -n "$NS" --reset-values -f "$OLD_VALUES" \
  --set-string "image.ref=$OLD_IMAGE" --set-string "argo.namespace=$ARGO_NS" \
  --set-string auditAnchor.rekorURL=
"${k[@]}" get deploy/oberth -o json > "$WORK/staged-old.json"
"${k[@]}" get pods -l 'app.kubernetes.io/name=oberth,app.kubernetes.io/instance=oberth' \
  -o json > "$WORK/staged-pods.json"
python3 - "$WORK/staged-old.json" "$WORK/staged-pods.json" "$OLD_IMAGE" <<'PY'
import json, sys
d = json.load(open(sys.argv[1])); assert d['spec']['replicas'] == 0
assert not json.load(open(sys.argv[2]))['items']
assert next(c['image'] for c in d['spec']['template']['spec']['containers']
            if c['name'] == 'oberth') == sys.argv[3]
PY
"${k[@]}" get cm -l oberth.ci/audit-witness -o json > "$WORK/current-witnesses.json"
python3 - "$PLAIN/before36.sqlite" "$WORK/current-witnesses.json" <<'PY'
import json, pathlib, sqlite3, sys
with sqlite3.connect(pathlib.Path(sys.argv[1]).resolve().as_uri()+'?mode=ro', uri=True) as db:
    assert db.execute('select max(version) from schema_migrations').fetchone()[0] == 36
    for cm in json.load(open(sys.argv[2]))['items']:
        kind = cm['metadata']['labels']['oberth.ci/audit-witness']
        assert kind in ('continuity', 'intent'), 'Unknown witness evidence: stop'
        r = json.loads(cm['data']['record.json'])
        row = db.execute('select sha256 from audit_actions where id=?', (r['auditID'],)).fetchone()
        assert row is not None and row[0].hex() == r['auditSHA256'].lower(), 'Witness outside backup: use forward repair'
        assert kind != 'intent', 'Pending intent: reconcile before restore'
PY
```

The local prefix screen is necessary, not sufficient. Before restore, the witness
operator must verify authoritative current remote history/receipts and reconcile
pending intents according to the [existing continuity procedure](../v073-from67-upgrade-runbook.md#the-witness-deadline).
A publication outside the backup prefix can close rollback. Unavailable history
or an unprovable prefix requires forward repair; never reset witnesses, delete
continuity ConfigMaps or disable verification to proceed. Stop at zero if the
restore gate refuses. Do not attempt the old image against schema 37.
If the copy is still schema 37 and the prefix screen, restore dry-run or other
pre-swap check refuses, recover this disposable target forward by reapplying the
new pinned chart/image, preserving its database and identities:

```bash
guard_rehearsal
# Recovery branch only: first confirm no restore swap occurred and the stopped
# disposable database is still schema 37. This exits the failed rehearsal.
"${h[@]}" upgrade oberth "$NEW_CHART" -n "$NS" --reset-values -f "$NEW_VALUES" \
  --set-string "image.ref=$NEW_IMAGE" --set argo.resourceSampler.enabled=false \
  --set-string "argo.namespace=$ARGO_NS" --set-string auditAnchor.rekorURL=
"${k[@]}" rollout status deploy/oberth --timeout=180s
exit 1  # Failed rollback qualification; do not continue to restore.
```

This recovery branch is not part of the successful sequence. If `set -e` has
exited, re-enter the target/pin variables, guard and array definitions through
`c=(...)` in a fresh recovery shell and recheck both CLI hashes. Reuse existing
evidence; skip fresh-directory/keygen/backup setup. Forward recovery reads no
backup private key. After any uncertain or post-swap failure, establish the actual image/schema from retained restore
evidence and the stopped disposable PVC before applying an image. Never reset
witnesses to force rollback or point this branch at production.

```bash
guard_rehearsal
sha256sum -c "$WORK/backup36.sha256"
# KEY USE 5: dry-run also decrypts and reads the throwaway KEY.
"$OLD_CLI" restore "$BACKUP36" --only history --private-key "$KEY" --chart "$OLD_CHART" \
  --max-database-bytes "$MAX_DB_BYTES" "${c[@]}" --timeout=180s --dry-run
# Review: no production namespace or Workflow may be in the plan.
guard_rehearsal
# KEY USE 6: real history restore decrypts with the throwaway KEY.
"$OLD_CLI" restore "$BACKUP36" --only history --private-key "$KEY" --chart "$OLD_CHART" \
  --max-database-bytes "$MAX_DB_BYTES" "${c[@]}" --timeout=180s --yes
"${k[@]}" rollout status deploy/oberth --timeout=180s
"${h[@]}" upgrade oberth "$OLD_CHART" -n "$NS" --reset-values -f "$OLD_VALUES" \
  --set-string "image.ref=$OLD_IMAGE" --set-string "argo.namespace=$ARGO_NS" \
  --set-string auditAnchor.rekorURL=
"${k[@]}" rollout status deploy/oberth --timeout=180s
"${k[@]}" get deploy/oberth -o json > "$WORK/restored36-deployment.json"
python3 - "$WORK/restored36-deployment.json" "$OLD_IMAGE" <<'PY'
import json, sys
d = json.load(open(sys.argv[1])); assert d['spec']['replicas'] == 1
assert next(c['image'] for c in d['spec']['template']['spec']['containers']
            if c['name'] == 'oberth') == sys.argv[2]
PY
"$OLD_CLI" backup --history-only --recipient "$CERT" --output "$WORK/restored36.backup" \
  --max-database-bytes "$MAX_DB_BYTES" "${c[@]}"
# KEY USE 7-8: restore --inspect and openssl cms -decrypt read the throwaway KEY.
inspect_history "$OLD_CLI" "$WORK/restored36.backup" 36 restored36
python3 - "$WORK" "$PLAIN" <<'PY'
import json, pathlib, sqlite3, sys
work, plain = (pathlib.Path(p) for p in sys.argv[1:])
def connect(label): return sqlite3.connect((plain/(label+'.sqlite')).resolve().as_uri()+'?mode=ro', uri=True)
with connect('before36') as before, connect('after37') as new, connect('restored36') as restored:
    ledger = dict(before.execute('select version,applied_at from schema_migrations'))
    assert all(dict(new.execute('select version,applied_at from schema_migrations'))[v] == at for v, at in ledger.items())
    assert dict(restored.execute('select version,applied_at from schema_migrations')) == ledger
    head = json.load(open(work/'before36.manifest.json'))['database']
    for db in (new, restored):
        if head['audit_head_id']:
            row = db.execute('select sha256 from audit_actions where id=?', (head['audit_head_id'],)).fetchone()
            assert row is not None and row[0].hex() == head['audit_head_sha256'].lower()
print('PASS: actual schema 36 -> 37 -> restored 36; original ledger/audit prefix retained')
PY
```

Require one old Ready server, original identities, authenticated unsealed
OpenBao/token delivery and reconciled Workflow/VM/helper cleanup before resuming
intake. History-only restore preserves OpenBao but discards post-backup DB writes;
retain restore receipts and `/data/pre-restore-*` evidence. If a swap/rollout
fails, keep writers stopped until the actual image/schema is understood.
Archive exact pins/chart diffs, renders, backup hashes/inspection, old-refusal
output, original and restored ledger/audit-prefix evidence, restore receipt,
health/cleanup acceptance and any failure. Archive only selected nonsecret
receipts and encrypted backups, not `installed-values.json`, plaintext scratch
or the throwaway private key. Keep values under the disposable install's private
retention policy. Remove temporary plaintext immediately after the checks; the
same cleanup runs on early exit or interruption:

```bash
cleanup_plaintext
test ! -e "$PLAIN"
```

After acceptance, dispose of the disposable installation and rehearsal-only key
directory under the coordinator's policy. Keep the throwaway pair separately
only as long as retained encrypted rehearsal backups need reinspection; never
add it to the production backup-key store. Unlinking scratch files is not a
claim of sanitizing filesystem snapshots. This procedure is prepared for the
coordinator; it has not been executed by this change.
