# Helm Service ownership migration

The normal Oberth Service shape is `NodePort`, with SSH exposed at Service
port `22` and fixed NodePort `30022`. If an existing release is already live
as `LoadBalancer` / `2222` / `30022`, do not jump directly to the normal
shape.

Kubernetes strategically merges `Service.spec.ports` by `port`, not by
`name`. Changing the SSH port therefore changes the list merge key. When the
last Helm manifest says `22` but the live Service says `2222`, a direct render
of `22` cannot safely establish ownership of the live key and can produce the
duplicate/order conflict that blocks a normal upgrade.

Use two normal client-side Helm revisions with the same chart and the same
non-Service value files/flags used for the installation. In the examples,
`values-production.yaml` represents that existing complete, non-secret values
input and `./oberth.tgz` represents the target chart package. The first
revision must render the exact live shape:

```bash
helm upgrade oberth ./oberth.tgz --namespace oberth \
  -f values-production.yaml \
  --set service.type=LoadBalancer \
  --set service.sshPort=2222 \
  --set service.sshNodePort=30022
```

Confirm that revision succeeds before starting the second. It adopts the live
SSH merge key without changing the NodePort or traffic path. Then create a
second ordinary revision for the steady state:

```bash
helm upgrade oberth ./oberth.tgz --namespace oberth \
  -f values-production.yaml \
  --set service.type=NodePort \
  --set service.sshPort=22 \
  --set service.sshNodePort=30022
```

The second revision is now an owned transition from port `2222` to `22` while
NodePort `30022` and the named `ssh` target remain stable. Do not combine the
two revisions, and do not use Helm force/replace, deletion, hooks, `kubectl`,
or an API-mutating workaround. Those paths either recreate the Service or
bypass Helm ownership and can interrupt SSH availability.
