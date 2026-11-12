# Declared resources

Write native `resources.requests` and `resources.limits` in the repository's
Argo container, script, containerSet, init-container and sidecar definitions.
Oberth preserves those declarations. It has no size/resource grant, measured
request resizing, default request fill or CPU/memory admission ceiling.
Missing requests are zero for Kubernetes packing, subject to Kubernetes' own
limit-to-request defaults and namespace policy.

Use `oberth run resources` to inspect read-only observations and commit any
resource changes to Git. Measurement never changes the submitted declaration.
Device/resource-template security capabilities remain administrator controlled.
See [native scheduling](native-scheduling.md).
