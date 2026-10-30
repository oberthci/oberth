# Schema-35 emergency restore qualification

The named operator procedure is now [the v0.17.73 runbook](v073-upgrade-runbook.md),
not an unspecified offline restore. This qualification supersedes the earlier
store-API-only claim. **Rollback is verified pre-upgrade database restore only.**
No index/ledger edits and no ordinary Helm rollback against schema 35.

## Real operator-path rehearsal, 2026-10-30

A new, disposable kind cluster `oberth-schema35-rehearsal` ran real Helm 4.3.1,
Kubernetes v1.37.0, a PVC, and actual Oberth servers. Source pins:

- Old CLI/server: released v0.17.72 `3f9cac207e235235d84a3965c90837e24f40a062`, schema 34.
- New CLI/server: approved RCA UX R2 `f9cbe94297c37a87b86dbb22158b36820a9f0d1a`, schema 35.
- Linux/arm64 server builds: Go 1.26.10, CGO disabled; FROM-scratch Git substrate
  reused from the previously qualified local image, replacing only Oberth.
  Darwin/arm64 operator CLIs: Go 1.27.2. Exact binary/image hashes in JSON.
- Old/new chart templates/values schemas from those exact commits; only chart
  version/appVersion metadata labelled the local lab versions. These are
  rehearsal builds, not an assembled/published v0.17.73 release image.
- Digest-pinned preloaded OpenBao 2.6.1 and BusyBox utility images. Preparation
  downloaded pinned images; the exercise used local cluster endpoints and
  preloaded images, without a production cluster or external witness. This
  was not a network-egress-denied cluster test.
- A real disposable OpenBao dev instance, Kubernetes auth role and generated
  installation identities were used. No production key retrieval, CA rotation
  or production OpenBao disaster recovery was exercised.

The original proposed sequence failed on a real chart: `replicaCount=0` is
rejected in both JSON schema and template validation. Skipping one validation
would not fix it. The successful sequence uses a disposable old-chart
maintenance copy whose **only** source difference is Deployment `replicas: 0`.
Values remain at the required one. Both the real applied Deployment and Pod
list were checked: exact old image, zero desired replicas, no server Pod,
before any database replacement. No old server briefly ran against schema 35.

| Rehearsed operation | Observed result |
| --- | --- |
| Real old server + CLI history-only backup to a fresh RSA recipient | CMS AES-GCM encrypted archive, verified actual schema 34, nonempty audit |
| Old CLI `restore --inspect` | Full envelope/member/database integrity/FK/audit validation passed |
| Helm upgrade to actual new server, record new audit data, new CLI backup | Actual snapshot schema 35, original prefix plus post-upgrade audit |
| Stop writers and stage the old maintenance chart with old values | Deployment old image/replicas 0, zero server Pods |
| Old CLI `restore --only history --dry-run` | Concrete swap plan and orphan deletion, no mutation |
| Same old CLI command with `--yes` | Helper validates staged old image schema, uploads/verifies/swaps DB, retains original files, starts one old Pod |
| Old rollout + version support + independent new backup/inspect | Ready, pinned old image, actual DB schema 34 with valid integrity/FKs/audit |
| Re-apply unmodified normal old chart/values | Remains one old Ready Pod and schema 34 |
| Real post-backup Workflow absent from restored DB | Deleted by the real restore CLI (no Workflow controller or task execution in this fixture) |
| Decrypt independently verified archives and compare audit rows/hashes | Exact original nonempty prefix preserved, post-upgrade audit data absent, restore.completed appended |
| Repeat using the runbook's literal Bash blocks, inserting a real upgrade between them | Bash syntax checks and whole sequence passed; schema 34 -> 35 -> 34 |
| Witness-prefix screen | Empty local evidence/matching completion pass; ahead head, wrong hash, pending intent and unknown evidence refuse |

First pass: backup held 2 audit entries, upgraded snapshot 4, restored snapshot
3 (original two plus restore.completed). Literal-Bash pass: backup held 4,
restored 5. This demonstrates real loss of post-backup audit history, not
preservation of post-upgrade changes. Previous database files remain separately
under `/data/pre-restore-<timestamp>/`; the restore does not turn them into
schema 34. Compatibility identities and OpenBao data were retained through
`--only history`. A read-only helper inspected both retained original databases:
both remain schema 35, keep runs_latest_ref_idx and pass integrity checks. The
throwaway cluster was removed after evidence collection; existing clusters were
not changed.

## Witness deadline and operational limits

The first Rekor publication outside the backup's audit prefix can close
ordinary restore. That can precede the upgrade, since backup records its own
completion after its snapshot. Pending publication intents are uncertain
external writes until reconciled. The runbook names the live argument/Helm
setting, immutable continuity/intent ConfigMaps, exact backup prefix screen and
required authenticated remote-history verification. Its local screen is a
necessary refusal check, not Rekor receipt-signature verification or proof of
complete remote history. This rehearsal configured local audit mode; no real
external Rekor publication/rollback or production witness authorization was
performed. An enabled installation with unprovable continuity must choose
forward repair, without resetting/disabling witnesses.

History restore drops newer runs, tokens, uplinks and audit records and deletes
orphan Workflows; it does not reverse external publishes/VMs/resources. The CLI
rehearsal used no run executions, secrets grants or external publishes. The
separate closed-copy store experiment below covers nonempty run-row loss and
old-version refusal. History-only backup/restore does not recover OpenBao KV,
policies/grants, chart values, identities or log/artifact files. Retain separate
recovery material and preserve current DB/WAL/SHM and ownership before recovery.
Actual production storage/PVC capacity/failure behavior needs its own rehearsal.

## Evidence delivered with the handoff

`SCHEMA35-RESTORE-CLI-QUALIFICATION.log` contains real commands, the failed
replicaCount attempt and successful corrected restore, literal-Bash execution,
independent archive checks and preserved-file checks.
`SCHEMA35-RESTORE-CLI-QUALIFICATION.json` records exact pins/hashes, schema/audit
checks, zero-replica/image/Pod assertions and witness-screen vectors. Runners,
source hashes and the earlier missing store qualification log/JSON are included
in `schema35-runbook-handoff.tar.gz`; its manifest records SHA256s. No private
keys, kubeconfig, database, encrypted backup, server binary or image is in that
handoff archive. The coordinator owns final image/chart pins and deployment.

## Earlier closed-copy store qualification (retained, narrower evidence)

The Linux Go1.26.10 store probes used WriteVerifiedBackup/VerifyBackupArchive:
schema34 with one run/audit entry -> schema35 with three runs/audit entries ->
old implementation refuses schema35 -> verified backup34 restores one run and
its exact original audit head. Corrupted and schema35 archives were refused by
the old verifier. Independent SQLite integrity/FK/schema/index checks passed.
No migration ledger or index edits were used. This established database/schema
compatibility but did not exercise `oberth restore` or Helm; the operator-path
rehearsal above supplies that missing evidence.

Retained evidence names: `schema35-restore-qualification-linux.log` and
`schema35-restore-qualification-linux.json` (both now explicitly packaged),
`schema35-restore-qualify-linux.py`, and exact old/new probe sources. The
plaintext synthetic archives are test artifacts, not operator backup guidance.
