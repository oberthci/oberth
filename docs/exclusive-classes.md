# Exclusive classes

Exclusive classes were retired in v0.18.0. The `oberth.ci/exclusive` annotation is
accepted and ignored; shared step limits are native Argo semaphores with limits
defined in the ConfigMap `oberth-sync-limits`, rendered by the argo-workflows
release since v0.20.0 ([where limits live](where-limits-live.md)). See
[admission and native synchronization](native-synchronization.md), section
"Migration from `oberth.ci/exclusive`".
