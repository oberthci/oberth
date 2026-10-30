# tuxbox first-hours watch and changes since v0.17.67

The reviewed host baseline is v0.17.67/schema33, capacity **4**, IO full
avg60 about **15.5%** and IO some avg60 about **35.7%**. The admission gate
uses IO **full**, not IO some: those measurements do not trip its default
30% threshold. Defaults are IO full30%, hysteresis10 percentage points
(resume strictly below20%), CPU some threshold0 (disabled), maxDefer20m.
There is no startup grace: a first eligible admission sample at or above30%
can immediately hold new grants. A per-run pressure clock starts only once
eligible; an unstarted clock cannot expire. Expiry removes only the pressure
hold; capacity, disk floor, ownership, priority and backfill gates still apply.

Proposed first-hours policy: preserve the reviewed capacity4, enable runtime
capacity, keep these PSI defaults and monitor. This is a documented proposal,
not authorization to change tuxbox. The coordinator may choose overrides before
production; record them explicitly in NEW_VALUES and the rendered ConfigMap.
During a long IO hold, **lower capacity; do not raise it**. Raising capacity
adds work when pressure clears or clocks expire. Lowering lets running grants
drain without cancelling them. Compare effective capacity/source in Status
with the projected ConfigMap; invalid values retain the last good setting.

## Liveness, readiness and observation

`/livez` is new when coming from67. More than five minutes without monitored
local writer/scheduler progress makes it fail. Its chart timeout15s, period60s,
failureThreshold3 normally restarts a stalled process around7–8+ minutes after
last progress, with probe/termination/scheduling jitter. Explicit external
waits are excluded; responsiveness errors and readiness failures do not by
themselves prove a local stall. Keep this probe enabled. Match chart and image:
a `/livez` chart with an old image lacking the route can repeatedly restart it.

Check rollout, Pod restart count/last termination, readiness transition logs
(reason/sample_age/db_wait), liveness component/age/error class, Status admission
holds and host `/proc/pressure/io`. Do not treat IO some as IO full. Compare
observed progress with the workflow deadline; deadlines already killed work
before this release, while status now explains deadline-exceeded and lag.

Readiness uses a dedicated read-only connection, current sample budget30s,
5s cadence and bounded90s age of the last successful sample; a delayed sample
does not renew that timestamp. Hard failures remain unready. A repeated writer
busy episode has its own90s completed-probe confirmation; six five-second
readiness failures make endpoint removal typically around two minutes after
initial busy evidence, with phase/load delay. The single API/SSH Service remains
readiness gated: no SSH Service/NodePort/DNS/host-key migration is included.
`publishNotReadyAddresses` is only a future option. The cached HTTP probes do no
DB/network work. Monitor rather than weakening probes to hide an overload.

## Cumulative v0.17.68–72 behavior to account for

- A direct67 upgrade applies **34** (queue_priority_patterns and
  run_priority_overrides) and **35** (runs_latest_ref_idx), both with ledger
  entries. No Helm rollback to67 works against either newer schema. Preserve a
  fresh verified schema33 backup and follow the rehearsed old-chart/old-CLI
  history restore; this loses post-backup DB changes and is subject to witness
  continuity. The former schema34-only runbook is not the tuxbox procedure.
- General reads use eight read-only WAL connections, audit reads two, and
  readiness/idle sampling are reserved. SSH and bearer auth share a separate
  four-connection pool with three admission slots each, preserving at least one
  for the other protocol. Tokens/scopes/revocation are freshly read; last-used
  timestamp writes are throttled to once per minute. Clean transient audit-fence
  contention retries; identity/proof/tampering failures stay hard refusals.
- Run/log reads have bounded deadlines/scan/output budgets. `byte_offset` follows
  `next_byte_offset` while `has_more` is true; use offset0 with the same
  tail/pattern/context. Empty intermediate pages do not prove absence; counts
  can be call-local lower bounds. Running logs have a bounded incremental
  index that extends with new bytes, keeps newest ranges on overflow and is
  dropped on finish. Pattern scans do not block CI issue-excerpt construction.
- Tag rerun without force requires complete trustworthy proof that no secret,
  WIF/credentialed or publication step started, plus current tag/chain ownership.
  Missing/old evidence fails closed. Infrastructure-failed tag retry is possible
  only under the guarded retry rules; uncertainty is not replay permission.
- CI lint waits are bounded, and remaining workflow deadline/observation lag is
  visible. Promotions and only the oldest waiting server-classified tag/release
  per repository receive automatic highest priority. No grant record defaults
  to this boost within L; an explicit size revoke disables it, even for a
  never-granted repository. L/XL grants re-enable it within that maximum.
  Exact checkout can withhold the boost when the declared size exceeds the
  repository maximum; claimed but unadmitted tags count as waiting. Inspect
  existing revoked grants before deployment. Already waiting normal/high runs
  aged 60 minutes jump to highest in FIFO order. Priority-class backfill means less lower-class
  backfill. Grants are not preempted; watch queue latency after the first tag.
- MCP issue mutations return compact acknowledgments; follow issue_get for the
  body. Issue field/body limits and compact run summaries reduce context.
  Go-proxy explicit versions retain first-observed bytes across tag moves or
  deletion; deletion alone does not revoke cached content. An administrator's
  durable per-version revocation marker denies warm/cold reads and discovery
  across restart. Preserve markers during cache reclamation; already downloaded
  client bytes require separate remediation. See goproxy-availability.md.
- Workflow create-timeout settlement distinguishes unsubmitted work from an
  uncertain POST, adopts first and avoids duplicate execution. Executor-identity
  infrastructure errors use bounded same-SHA retry only where allowed. Check
  durable run/workflow state before repeating a mutation with a lost reply.
- **Drain all KubeVirt/resource-delete runs before upgrade**, not merely runs
  using cleanup-verdict. Older submitted workflows can fail across the upgrade
  because deletes now run through server-mediated ownership/UID checks and RBAC
  changes. Wait for terminal cleanup, or explicitly accept a later rerun after
  reviewing credential/publication evidence. History restore does not undo VM,
  resource or publication side effects. Preserve OpenBao state separately.

## Known limits to carry into release notes

An adopted run whose Pod has already been garbage-collected can show
`interrupted` even if a real init failure occurred and its evidence disappeared.
Observable nonzero init/step failures remain failed. The new minimum180s
Workflow retention assists final reads only for newly submitted workflows;
it does not retrofit old Workflows or cover arbitrarily long downtime.

After host crash, durable journal replay occurs through observeRun only; other
startup/finalization/read paths do not universally replay it. A writer before
a restartable init reader is forced read-only and can fail its init write;
that shape fails closed. These limitations do not authorize automatic replay
of credentialed/publication work or unsafe writable mounts.
