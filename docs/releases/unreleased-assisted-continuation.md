# Unreleased: agent-assisted CI continuation (retry hold)

**MCP tools: 85 → 90** (`run_hold_list`, `run_hold_exec`, `run_hold_patch`,
`run_continue`, `run_hold_abandon`). `run_rerun` gains `assist`; the CLI gains
`oberth run rerun --assist` and `oberth run hold list|exec|patch|continue|abandon`.

Off by default. With `assist.enabled`, a branch CI run pushed with
`git push -o oberth.assist=1` (or rerun with assist) parks the retry of a
failed credential-free step before its command: Build adds a server-owned
one-attempt `retryStrategy` and a conditional `podSpecPatch` that pauses the
Argo emissary on the retry only, widens the kubelet deadline by `assist.holdTTL`,
makes `/work/src` writable and detaches the shared caches. The run's agent
inspects the live environment (`run_hold_exec`, argv-only), applies a unified
diff through the server's own Pod helper (`run_hold_patch`), and releases the
step (`run_continue`) so Argo continues the DAG natively; `run_hold_abandon`
fails it. Every action is audited (`run.assist.*`) and appended to the run.

A run with any exec, patch or continue on a hold is **assisted** (recorded
before the action runs): never a promotion source, never
published upstream, never green-for-SHA or CI-issue evidence, with the reason
line `assisted: step X continued after patch sha256:… by <actor>; not evidence`
on its existing status. Release, tag, promotion, schedule, plan and apply runs
never hold. Only a command that failed on its own is retried and held: the
injected retry expression refuses signal exit codes (137, 143) and any attempt
whose duration reached the floored step budget, because Argo v4.0.8 records a
kubelet `activeDeadlineSeconds` kill with exit code 143, not `-1` (found in
the live spike; a timed-out step would otherwise have been parked and rerun
with a second budget). #1499 classification and the infrastructure retry are
unchanged.

Helm: `assist.{enabled,holdTTL,maxConcurrentHolds,execTimeout,execOutputBytes,patchMaxBytes,patchMaxFiles}`.
Admission now refuses `ARGO_*` environment names from repository YAML.
Schema: migration `assisted-runs` adds `runs.assist_requested`, `runs.assisted`,
`runs.assist_record` (rollback: pre-upgrade backup). No new RBAC.
See [assisted continuation](../assisted-continuation.md).
