# Declared resources

Write native `resources.requests` and `resources.limits` in the repository's
Argo container, script, containerSet, init-container and sidecar definitions.
Oberth preserves those declarations. It has no size/resource grant, measured
request resizing, default request fill or CPU/memory admission ceiling.
Missing requests are zero for Kubernetes packing, subject to Kubernetes' own
limit-to-request defaults and namespace policy.

Usage figures and resource reports are absent in v0.22.0. Commit resource
changes to Git and inspect Kubernetes metrics directly. GPUs and other extended
resources need no Oberth grant; operators enforce namespace ResourceQuota.
Resource-template API-kind/action capabilities remain administrator controlled.
See [native scheduling](native-scheduling.md).
