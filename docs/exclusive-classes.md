# Exclusive classes

Oberth has no exclusive-class scheduler. Use native Argo mutexes or semaphores
on the Workflow or template; operators create and maintain semaphore limits.
Retired Workflow annotations have no scheduling effect. Template Pod metadata
follows ordinary annotation security rules. See
[native synchronization](native-synchronization.md) and
[native scheduling](native-scheduling.md).
