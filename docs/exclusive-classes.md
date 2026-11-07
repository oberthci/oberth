# Server-wide exclusive classes

Some steps use a resource that only one run on the server may use at a time,
for example a host device or a single shared lab environment. A
[concurrency group](concurrency-groups.md) lets cooperating repositories agree
on a limit in YAML. An exclusive class is the administrator-owned form: the
server defines the class and its limit, and repositories can only queue behind
each other in it (#1499).

## Administrator configuration

The feature is off until a class is configured. Chart values:

```yaml
admission:
  exclusiveClasses:
    kvm: 1        # at most one run with a kvm step at a time
    gpu-lab: 2
```

This renders `--admission-exclusive-classes=gpu-lab=2,kvm=1`. A class name is
1 to 40 lowercase letters, digits and inner hyphens; the limit is 1 to 64
(default 1 on the flag when omitted); at most 32 classes. Changing the list
requires a server restart, like the other admission flags.

## Declaring a class in a step

A step declares the class on its template metadata:

```yaml
templates:
  - name: vm-test
    metadata:
      annotations:
        oberth.ci/exclusive: kvm
    container:
      image: ...
```

Inline templates and `spec.templateDefaults` may declare it too. A pipeline may
declare at most four distinct classes. Declare it in the repository's own
pipeline document: the scheduler reads the immutable checked-out document
before admission.

A class that is not on the server's allowlist (including every class while the
list is empty) refuses the run at admission with a message naming the class.
The refusal is deterministic, so it is not automatically retried.

## Behavior

Membership is per run: a run is a member of every class one of its steps
declares, for its whole execution. At most `limit` member runs hold a class at
once, across every repository and every trigger. Further members wait in
Oberth's own admission queue in priority/FIFO order within the class. A
waiting member owns no workflow slot and creates no Workflow or Pod, so nothing
sits Pending in Kubernetes, and runs outside the class are admitted past it.
Membership never bypasses the global workflow ceiling, administrative hold,
disk floor or pressure valve. A run may use both a concurrency group and
exclusive classes; it waits until all are free.

The queued reason reads `exclusive kvm busy`; detailed status reads
`Waiting for exclusive class "kvm" (limit 1); runs outside the class are
admitted meanwhile.` Holder identities are not shown, so scoped viewers learn
nothing about other repositories. The admission observation adds
`exclusive_classes`, `exclusive_class`, `exclusive_limit` and
`exclusive_blocked`. No run status, label or layout is added.

On restart, adopted running Workflows keep their classes (read from the live
Workflow), even if the class was removed from the list or its limit lowered:
running work is never interrupted, and new members wait until the class drains
below its limit. Uncertain Workflow creates retain their classes the same way.

## Security and abuse bounds

The class is a server-owned admission queue. It is not an Argo
synchronization object, ConfigMap semaphore, node selector or affinity, which
all remain refused, and it adds no node or cluster reach. Repository-chosen
names never become Kubernetes objects. The allowlist bounds abuse: a
repository can only join a class the administrator created, and a member holds
it at most for its workflow deadline. A repository that declares a class can
delay other members of that class; it cannot affect runs outside it. Keep the
list empty unless a shared resource needs it.
