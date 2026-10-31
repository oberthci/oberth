# Measured peaks beside Workflow size labels

This is a report-only follow-up to the opt-in trusted sampler. Admission still
uses the existing S/M/L/XL classes and administrator weights. Nothing in the
report changes a grant, authored request/limit, Workflow, retry policy or size
classification. It supplies no automatic request recommendation.

`run_get`/the full run HTTP response attaches `MeasuredSizeComparison` to each
assembled step beside `MeasuredUsage`. MCP `repo_usage` adds `report_only:true`,
`size_class_semantics` and each step's `by_workflow_size` list. Each list has at
most five buckets: S, M, L, XL, unknown. Existing repository/time/step filters,
1,000/default–5,000/max observation window, 128 step-group cap and run-view
256-observation cap still apply, with their existing truncation indicators.
No additional query, table, migration or scan window is introduced by this view.
The stage1 prerequisite still applies schema36 even with sampling disabled.

Each bucket reports observation count, OOM-killed, unsampled and flagged counts,
plus known-value count/p50/p95/max for main-container CPU interval peaks and
memory peaks, and pod-wide ephemeral-storage peaks. Units are nanocores and
bytes; the field names state the scope. Main and pod values are not added.
Unknown values remain null/count0; a measured zero remains a counted zero.
Counts are container incarnations in the bounded selected window, not an entire
repository population, independent jobs, or a complete Workflow census.

The size label comes from the verified observed Workflow declaration. A
size-by-trigger annotation requires one valid server-bound oberth-trigger
parameter; otherwise that observation's class is unknown. This cannot recover
an unstamped trigger for old submitted objects or new Workflows declaring
`oberth.ci/size-by-trigger` without declaring or referencing `oberth-trigger`
in their spec: the server stamps the parameter only when the spec uses it.
A valid ordinary Workflow with no size annotation uses its documented M default.
Old stored observations with no class remain unknown; a later trusted
observation may fill it. Conflicting
labels clear it permanently and flag the observation without losing measured
usage. Termination-message/progress-declared classes are never used as trusted
comparison labels, and no Pod/Workflow spec is changed to add reporting metadata.

A Workflow size is an **admission label**, not a numeric per-step CPU/memory
budget. The code defines no universal resource envelope for S/M/L/XL, so this
report groups measured peaks within labels instead of manufacturing a smaller
recommended class. Mixed pipeline/image implementations can share a label and
burn/step; do not treat those aggregates as an approved policy or training set.

All peaks are sampled lower bounds. OOM/killed/unsampled attempts and missing
helper, sidecar, ContainerSet or VM coverage can conceal higher demand. A CPU
interval rate is not instantaneous CPU demand. Pod ephemeral usage excludes
source PVCs/shared caches/VM disks; main memory is not whole-Workflow/VM memory.
The existing used/request ratios compare the observed metric's documented
scope only. No low quantile, missing sample, high pressure or reported small
peak authorizes lowering requests/limits or increasing admission capacity.

The sampler remains disabled by default and still needs direct-kubelet
TLS/token/RBAC/metric qualification. Collection failure leaves normal run views
usable. Platform protection and a representative coverage window precede any
later request policy or whole-run vector admission. See
[the measured-requests design](../DESIGN-measured-requests.md),
[platform protection](platform-protection-plan.md) and
[PSI observation/enforcement](io-pressure-admission.md).
