# From v0.17.67: rollback qualification

PASS: actual published v0.17.67/schema33 → exact863a88eb/schema35 → actual
v0.17.67/schema33, in isolated kind with a disposable real OpenBao fixture.
The candidate code branch remains **863a88eb80244f0d7cc8ee96852e3e1e64381f89**.
This docs-only follow-up does not change it or rebuild its artifacts.

The actual old CLI and image are bound by independently verified signed
release metadata. Old source is 5e567d5c1f60c001c43bc4d07a9255bac44f4b25; the official image index
is `europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:86070b32caf9c301bddb69515dcaceceb7c57483b743686ba002ccfa9ea0cc7d`. Message signatures were verified with the source-pinned
release public key; separate OCI signatures and transparency-log proofs were
not verified. Both downloaded old arm64 CLIs matched signed checksums. The
new image/chart/operator CLI are the unchanged exact-head local artifacts from
the previous final-pin rehearsal; no published/signed73 image is claimed.

| Verified archive | Actual schema | Audit rows | Source |
| --- | --- | --- | --- |
| Fresh pre-upgrade history | 33 | 1 | Published v0.17.67 /5e567d5c1f60 |
| New-server history | 35 | 4 | Exact863a88eb |
| Restored old-server history | 33 | 2 | Published v0.17.67 /5e567d5c1f60 |

The original33 ledger timestamps survived upgrade unchanged. Migration34's two
priority tables and migration35's covering index appeared only at35 and were
absent after whole-history restore. Each closed backup passed integrity,
foreign-key and audit-chain checks. The original audit prefix was byte-for-byte
preserved; an explicit post-upgrade audit marker was lost and restore.completed
was appended. This is the expected data-loss boundary, not a history merge.

The actual literal from-67 Bash commands ran in two fresh shells. All six
from-67 blocks and five retained from-72 blocks passed bash -n. Original
pre-upgrade evidence was not overwritten by emergency recovery. Before restore,
the hand-stamped old chart was staged at zero replicas with no old Pod against35;
its maintenance copy changed only the Deployment replica template. The actual
saved zero-replica state passed the old-image check and refused a wrong new
image. Restore dry-run and confirmed history restore both succeeded, an actual
post-backup orphan Workflow was deleted, and the old server became ready on its
original published image. A fresh old-CLI backup proved actual schema33.

A read-only nonroot PVC helper independently inspected the retained original
`/data/pre-restore-*` database: schema35/index and post-upgrade audit evidence
were intact. No SQL writes, index deletion or ledger edits were used. A schema35
archive was rejected by the actual old CLI. Synthetic guard tests refused a bad
old/new executable hash before execution, wrong old support schema, an outside
witness prefix and a pending witness intent. A matching prefix passes only the
necessary local screen, not external witness authorization.

Differences from the prior34→35→34 rehearsal: the old side is now the real
published67 release and its Git-tag chart (raw metadata0.17.5, hand-stamped67),
not a local72 build; backup/asserts target33; both migrations34+35 are exercised;
the operator CLI hash is checked before execution so the local62 CLI cannot be
substituted. The mechanics and single maintenance replica edit are unchanged.
Old chart archive SHA256: `de836d9ca99ce0f8af4dae9733f48672dc1e164c339bc0741d0b14b5226e73db`.
Qualified from-67 runbook SHA256: `602ce8538920fd011009d022962338d20c21f14160e2ecd44b0cdf407c46a7ec`.

Limits: fixture history has a nonempty audit chain but no runs/issues or external
publications. There is a minimal Workflow CRD for orphan deletion, no actual
Argo controller/KubeVirt workload, no production identities/PVC/keys and no
external Rekor. Real tuxbox witness policy, capacity4/PSI choice, resource-delete
cell and sustained physical-host IO need operational confirmation. First-hours
proposal preserves capacity4 and default full30/resume<20/maxDefer20m. Never
reset witnesses or disable liveness to make a rollback/overload appear healthy.
The disposable cluster was removed after qualification.

Complete logs, JSON, scripts and public signature evidence accompany
READY-v073-from67.md under /tmp/fix. Private keys, kubeconfig, databases,
encrypted fixture backups, binaries and images are excluded from the evidence
handoff archive. The earlier34→35→34 qualification is retained separately.
