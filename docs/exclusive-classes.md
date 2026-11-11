# Exclusive classes

Exclusive classes are gone. A document carrying the `oberth.ci/exclusive`
annotation is refused at admission; shared step limits are native Argo semaphores with limits
defined in the ConfigMap `oberth-sync-limits`, rendered by the argo-workflows
release since v0.20.0 ([where limits live](where-limits-live.md)). See
[admission and native synchronization](native-synchronization.md), section
"Migration from `oberth.ci/exclusive`".
