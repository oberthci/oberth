# Retained OB-83 objectives for the OB-84 laptop branch

Base: `44c2f0f6e615846d9197800f78b67c0b462dddd4` from `OB-84-BASE.bundle`.
Branch: `fix/ob-84-laptop`. This replays the retained `fix/ob-83-rest` work
onto the integrated v0.17.83 candidate. No old workspace implementation or
superseded review branch is merged.

The owner decision of 2026-11-03 keeps issues in Oberth and assigns the new
AI-workspace model, work statuses and stale handling to a separate worker.
External tracker migration and global-write retirement are no longer objectives.
Commits `65ba8037` and `48e01e56` are deliberately excluded; this branch adds no
issue-import policy, write-retirement switch or database migration. Migrations 49 and 50
are reserved for separate v0.17.84 inputs; any new migration here would start
at 51. Existing migrations 1–48 remain contiguous and unchanged.

This audit covers the same 33 identifiable issues (#1216, #1287, #1292–#1320,
#1398 and #1464). It is an implementation disposition, not a live tracker
closure report. No issue states or other repositories were changed.

## Retained acceptance clauses

| Objective | Delivered behavior | Replayed commit | Remaining live acceptance |
| --- | --- | --- | --- |
| #1216 / #1287 | Per-step sizing acceptance requires five distinct comparable terminal runs with complete CPU/memory/storage/duration and known limits together. Censored, mixed or incomplete history cannot justify reductions. CLI JSON/YAML and MCP expose the verdict. | `cc303f3e` (from `b7b262f4`) | Collect qualifying real runs and inspect the report. No workflow resource values were resized. |
| #1294 preparation | Private live check inventories all registered branch sources, exact pinned fragments, legacy declarations, cleanup journals and drain obligations. Refusal names the dependent source/record. Legacy recovery remains available at the preparatory head. | `77121496`, `daa0f614` (from `704e3fe2`, `95e82c57`) | Deploy preparation, migrate dependent pipelines, stop submissions, drain and confirm external cleanup, then back up. |
| #1294 final removal | Held for a later release. Legacy VM/KVM/guest/trusted execution, grants, RBAC and harness publishing remain available on this branch. Retirement inventory is on-demand only. | Original `03728183` explicitly excluded (review F2) | Install a published preparatory release first; migrate dependent repositories, drain and verify cleanup, then separately review and release the final removal. |
| #1292 | Document independent connector/site ownership and require a fresh cluster/release-bound handoff before upgrading a release that still owned those objects. Check connector readiness/UID/dependencies, explicit site evidence and independent credentials; recheck before first live write. | `bbc24198` (from `4806ce67`) | Transfer ownership/routing/publishing, verify traffic and supply the handoff receipt before replacing the owning release. |
| #1317 | Retain the full recorded issue map with the current owner decision and explicit live prerequisites. | This audit commit (adapted from `3a12278c`) | Closure depends on coordinator acceptance and the separately owned workspace input. |

The complete `fix/ob-84-laptop` branch is now the preparatory release input.
It retains legacy execution/recovery; no commit on this branch activates final
removal. Startup inventory has been removed, and the three-minute write deadline
applies only to the on-demand retirement-check admin route. The check is an
observation, not a durable submission pause. Retain the published preparatory
release and verified backup for rollback after a later removal release.
Connector/site removal already exists in v0.17.83, so its ownership handoff is
required even before a preparatory upgrade from an owning release.

## Entire recorded queue
Candidate commit/source evidence below was previously inspected against the
integrated candidate. The unchanged integrated fixes were retained, not replayed.

| Issue | Candidate implementation / source checked | Disposition on this branch |
| --- | --- | --- |
| #1216 | `e0db3394`; numeric declarations, `internal/resourcesampler`, `internal/measured`, resource reports and explicit admission ceilings | Coordinator-owned numeric admission retained; `cc303f3e` completes the requested per-step sizing acceptance in CLI JSON/YAML and MCP. Real-run qualification remains owner-operated. |
| #1287 | `run_resources` refreshed for 100 passed Oberth runs, 67 groups, 663 attempts | `cc303f3e` refuses a sizing decision until five distinct terminal runs jointly provide complete comparable CPU/memory/storage/duration/limits. Censored or mixed cohorts cannot justify reductions. No workflow requests/limits were resized. The earlier live report is not new acceptance evidence. |
| #1292 | `691d493e`, `15ebb279`, `2fd2344f`; website/tunnel removal, generic-name and private-address gates, deployment notes | `bbc24198` adds external ownership instructions and fail-closed upgrade proof bound to cluster UID, Helm revision/manifest, connector identity/dependencies and explicit site evidence. The coordinator must perform and attest the actual ownership/traffic transfer. |
| #1293 | `e14e4353`; deleted pilot/bridge packages and `pilot_retirement_schema.go` (schema 45), retained read-only journal | Integrated pilot removal retained. Shared legacy VM execution and recovery remain until the separately released #1294 final removal. |
| #1294 | `714ee610`, `6986c3ec`, `51df7085`, `330e524c`; capability grants/admission/identity, schema 46 and deferred schema 47 | `77121496` and `daa0f614` provide preparation and live verification. Startup never runs inventory. `03728183` is excluded; final removal requires a later published release and live migration/drain evidence. |
| #1295 | `e21f55cb` added installer env support; `a2c9b106` restored an implicit hosted endpoint | Remaining regression fixed in `4c989840`: explicit endpoint required before credentials or transport, fallback removed, contract/generic-name guards restored. Existing HTTPS guard verified. |
| #1296 | `b284ad8f`; Git default-branch refusal, `internal/service/push.go` deletion intent/outcome audit | Already present; no replay. |
| #1297 | `3ceded7c`; additive PKI role names, `serverpki_lifecycle.go`, preflight and explicit name removal | Already present; no replay. |
| #1298 | `f038178f`; `promptSecretStoreChoice` refuses unattended fresh setup without OpenBao and preserves existing setup | Already present; no replay. |
| #1299 | `db4b0a70`, integrated priority/backfill follow-ups; numeric admission diagnostics and reservation-aware backfill | Already present; no replay. |
| #1300 | `a2321455`; credentialed access-reconciler gate in `internal/app/argojobs.go` | Already present; no replay. |
| #1301 | `ec0aeeb0`; complete bounded artifact scanning, long-line redaction and documented issue-text boundary | Already present; no replay. |
| #1302 | `6dcd5f2c`; registry/repository boundary checks now under renamed `pkg/pipeline` | Already present; no replay. |
| #1303 | `499130f3`; served TLS renewal health, readiness and worker joins in `cmd/oberth/servertls.go` / `serve.go` | Already present; no replay. |
| #1304 | `a1b6656f`; classified startup retries, token/store readiness and deferred checkout cleanup | Already present; no replay. |
| #1305 | `59da2995`; stored install/upgrade settings, value preview and cancellation-aware waits | Already present; no replay. |
| #1306 | `9edee3bf`, `96715d82`, `4628d717`; credential capture/redaction, exact OpenBao ownership and remote-target checks | Already present; no replay. |
| #1307 | `eda1ecfd`; durable run IDs, tag recovery/adoption and publication retry authority | Already present; no replay. |
| #1308 | `00c48653`, `ee6dc85a`, `489e86b0`, `76eb963e`, `9c770583` and auth follow-ups; scoped grant wrapper, expiry, failed-auth limits/logging, orphan policy cleanup, private admin socket | Already present. Scope enforcement lives in `scopedPromotionGrants.RevokePromotionGrant`; the raw handler alone is not the boundary. |
| #1309 | `282d0ae4`, `702c57d8`, `1af25e9f`, `cd49997a`, `6549a1aa`, `bc72a934`; cancellable shared/exclusive locks, SSH inactivity, promotion pin retirement, quarantine and live-daemon cache removal | Runtime fixes already present; no replay. `d446f408` applies the established fixture timeout budget to two concurrency tests: real Linux checkout takes about 10 seconds here, exceeding their literal 1–2 second deadlines even without a competing operation. Both pass while transport remains stalled. |
| #1310 | `0c14bd38`; streamed module archive production and bounded subprocess/ref output in goproxy/gitcache | Already present; no replay. |
| #1311 | `935640b6`, `689b2985`, `4dc24c78`, `50fbdeb8`, `607e2bbe`, `3a09683f`, `28fd043d`; original failed step, terminal-only sizing, cron, log tails, YAML and shared status | Already present; no replay. |
| #1312 | `e3ef5fcb` through `1f9a9fc7` and related fixes; atomic issue hash/revision checks, dependency errors, 413 and compatible HTTP/MCP aliases | Base already includes `6568b452`, which checks the body hash inside the update transaction. This branch adds deterministic race qualification; HTTP/MCP aliases and metadata revisions remain unchanged. |
| #1313 | `f2361e47`, `ffd3075a`, `a7f01b69`, `90136603`; production publication heal, production database fixture path, live Git preparation, retired installer wrapper in tests | Already present. `serve.go` calls `openReconciledStartupDatabase`; audit verification uses the startup wrapper. |
| #1314 | `c00e44e2`, `27337a70`, `c15250ac`, `3fa7135f`, `d4b2c33f`, `f0037fa9`, `5798fb3d`; dead-code removal and exact exception gate | Integrated exact-exception gate retained. Existing legacy VM exceptions remain with the retained engine; remove them only with the later final removal. No new exception added. |
| #1315 | `880cce9a`, `ee0115b0`, `0f391042`, `a5d45507`, `fbcd1cfb`; Makefile, help/flags, archive docs, tree scan and installer output | Already present; no replay. |
| #1316 | Additional 2026-11-03 review of chart/RBAC, Docker build context, scripts and AGENT-CONTRACT | Fixes are included, adapted to retain the legacy engine and its four KubeVirt tools. Full commit/finding map and validation are in the delivery READY file. |
| #1317 | Owner triage umbrella; workspace remains absent, discarded workspace schemas refused (`0cb2a114`) | Owner decision of 2026-11-03 retains Oberth issues and assigns the new AI-workspace model to a separate worker. This branch supplies the retained sizing/removal/ownership work; no workspace feature is implemented here. |
| #1318 | Candidate has CI issue projection/locks and #1312 concurrency fixes, but no migration aid or feature audit | External tracker migration and global-write retirement are superseded by the owner decision. Commits `65ba8037` and `48e01e56` are excluded. Existing issue history and writes stay in Oberth; the new workspace model belongs to another worker. |
| #1319 | `71897545`, `f87a762b`; trusted workflow mismatch becomes a run-local failure, exact UID cleanup retained until absence | Candidate run-local trusted-workflow mismatch fix and exact UID cleanup remain active with legacy execution. Final removal is held for a later release. |
| #1320 | `f0402338`, `8b01b219`, `cd49997a`; audited unscoped-admin publication abandonment, delivery serialization, no push, quiet backoff and pin cleanup retry | Already present; no replay. |
| #1398 | `f62358c5`, bounded execution/deadline follow-ups; token wait progress, signal diagnostics and workflow/watchdog termination | Already present; no replay. No deployment was used to reproduce the live incident. |
| #1464 | Priority submission/grants, durable ordering/supersession guards and integrated EASY backfill | Coordinator-owned implementation retained. The live issue is closed with its v0.17.81 receipt; this worker did not deploy or independently certify that rollout. |

## Coordinator-owned live receipts

1. A per-step sizing report from five complete comparable real terminal runs.
   Prior history with unknown limits is not sufficient acceptance evidence.
2. Migrated registered branch and pinned-fragment sources, submission pause,
   drained preparatory queue/cancellations, clean legacy journals and external
   Kubernetes inventory, backup, then guarded final upgrade/readiness.
3. Independent connector/site ownership/dependencies, verified traffic and
   publishing, plus a fresh ownership handoff receipt where required.

Issues stay in Oberth. No external tracker choice, import or global read-only
cutover is required. The workspace model is a separate input, not built here.
Procedures: `docs/measured-requests.md`, `docs/legacy-removal.md`,
`docs/retirement-upgrade.md`, `docs/ownership-handoff.md`. Delivery hashes,
commit provenance and test receipts are in `/tmp/fix/OB-84-LAPTOP-READY.md`.
