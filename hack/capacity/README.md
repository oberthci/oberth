# Capacity maintenance and qualification

This is an operator runbook. Configuration candidates are unqualified until the
receipts below exist. Do not raise the run cap from this document's examples.
Retention/shorter TTL and permanent cap-guard retirement need a separate approved
protection-safe retention design. Keep the current TTL and collection barrier.

## S1a: host profile

The complete candidate drop-in is
[`kubelet-profile.yaml`](../../internal/installer/kubelet-profile.yaml).
It keeps maxPods 110, systemReserved 1500m/4Gi and enforcement on Pods only.
Do not add another reserve, reserved-cgroup enforcement, swap capacity or a CPU
governor change. The profile includes both filesystem inode thresholds because
partial hard-eviction maps disable omitted thresholds with default merging off.
The shared image/node filesystem needs no independently configured containerfs
threshold. Requalify the profile if the runtime's filesystem topology changes.

For the supported k3s version (>=1.32), the operator places the reviewed drop-in
after `00-k3s-defaults.conf` in the effective kubelet configuration directory
(normally `/var/lib/rancher/k3s/agent/etc/kubelet.conf.d/`). First save exact prior
drop-ins and effective public kubelet flags; remove conflicting kubelet CLI flags
from their actual owner configuration. Flags override drop-ins. Never edit the
generated containerd configuration or install a privileged host-mutator/purge cron.
References: [k3s configuration](https://docs.k3s.io/installation/configuration#kubelet-configuration-files),
[kubelet API](https://kubernetes.io/docs/reference/config-api/kubelet-config.v1beta1/).

Each disruptive layer needs a separate naturally drained window:

1. Save installed release/chart/images, rendered templates/checksums, values,
   public node config, quotas, namespaces and obligations. Verify a usable backup
   and selected administrator profile's unseal capability without displaying any
   recovery material. Assign observation and recovery ownership. Identify and
   disable the specific external cap-guard process and launch hook before
   maintenance; if this cannot be verified, defer. Do not restore reinstall logic
   on rollback. Permanent retirement remains blocked by retention prerequisites.
2. Coordinate pausing submissions and native schedules. Let existing Workflows,
   retry/hook/VM/Job children, held work, archives and publications finish. Check
   every affected namespace and outstanding obligation, not just Workflow phase.
   A positive lower cap only reduces new starts; zero means unlimited. Keep
   collection running. Do not force-cancel work to make the window.
3. For S1a only, apply the saved/reviewed drop-in and restart k3s once. Re-read
   node configz and status. ReviewHost must match the effective kubeletconfig;
   recompute allocatable from node status (10% nodefs reserve is approximately
   844Gi on the measured filesystem). A file on disk is not effective evidence.
4. Before reopening: node Ready and no pressure, store actually unsealed (its
   native readiness probe also accepts sealed state), server/controller Ready,
   source/grant checks and API probes passing for ten minutes. Inspect one
   ordinary run's identities, requests, events and archived logs before widening.
5. Host rollback restores exact prior drop-ins/flags at drain and restarts once;
   repeat effective-config/readiness checks. It cannot undo eviction or deleted
   runtime content. Never reinstall another release as a pressure response.

G7: sample node/Pod/status/events/API every 20s, directories every 5min and archive
receipts every minute. Stop expansion on any pressure, unexpected restart,
control OOM/eviction/readiness miss, API p95 >1s or >110% of busy baseline over
5min (>=15 samples), nodefs <20%, directory >=90% budget, or threatened drain
reserve. The hard free-space acceptance floor is 15%. Pause new arrivals, keep
collection running, drain safely and diagnose; reverse configuration at its
proper boundary. Archive backlog growing in two one-minute samples or a terminal
archive incomplete after five minutes also stops expansion. Require ten minutes
healthy before reopening and >=48h/two busy bursts for ordinary-work acceptance.
Missing G1 identities, high-water measurements or staffed observation blocks
qualification. Record planned maintenance separately from ordinary-work outages.

## S1b: controls

`install` and `upgrade --resources '<JSON>'` accept native requests/limits keyed
by server, controller, store and executor. The full installer `--detail` review
shows values. Change one component per drained window; preserve all other saved
choices and the installed release. The server value is `resources`; the other
values are `argoNative.controller.resources`, `storeNative.server.resources` and
`argoNative.executor.resources`. Defaults retain CPU/memory requests and the
store's 512Mi limit. No default CPU or new memory limit is imposed.

`control-candidates.json` contains unqualified ephemeral trials, not a combined
rollout. Select exactly one entry after G1 high-water/cold source measurements
qualify it; server tmp can reach 2Gi, and PVC state is separate. Record final Pod
requests/limits and exact old component template. Server/controller use Recreate;
store uses RollingUpdate. After a store restart use
`oberth --profile <saved-profile> unseal`, then repeat the same recorded
install/upgrade completion when required. Pod readiness alone cannot prove
unseal. Restore the exact prior resource values at drain for rollback; unseal
again if store restarts. Do not use release rollback for a resource-only trial.

Off-production G2 recovery fixture protocol (mandatory, not executed by render
tests): freeze the full Pod inventory and pinned versions; fill the qualified
aggregate run CPU/memory/Pod quota using supported children and largest inputs.
Retain native objects/events, 20s API samples, memory high-water/throttle and
pressure/OOM/eviction counters. Replace server, controller and store separately
at that full load and test worst permitted non-run/maintenance overlap, including
higher preempting system classes. Record replacement UIDs, scheduling and Ready
times, store seal/unseal and successful source/grant/log checks. Deliberately
under-request CPU/memory and exceed a run limit: a fault run may OOM; no control
OOM/eviction, node pressure or out-of-budget host/device usage may occur. Require
ten minutes healthy API recovery and an explicit return to previous values.
Unknown shapes, absent receipts or failed containment blocks activation.
