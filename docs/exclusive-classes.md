# Exclusive classes

Exclusive classes were retired in v0.18.0. The `oberth.ci/exclusive` annotation is
accepted and ignored; shared step limits are native Argo semaphores with limits
defined in `argoSync.semaphores`. See
[admission and native synchronization](native-synchronization.md), section
"Migration from `oberth.ci/exclusive`".
