# Platform protection

Oberth's own server Deployment keeps the shared control-plane PriorityClass
(value 1,000,000, `PreemptLowerPriority`). The class has
`globalDefault: false`; unrelated Pods do not inherit it. The server class
can preempt lower-priority Pods where Kubernetes permits it. It is not a
capacity guarantee.

New user Workflows receive no Oberth Pod priority class or preemption policy.
Repository Pod priority classes and arbitrary Pod patches remain refused;
authors can declare native Argo `spec.priority` for Workflow ordering.

Source and executor identity helper Pods use Kubernetes default priority. The
chart and installer create only the server's class. An old pipeline class may
remain unused because its earlier chart carried a `keep` annotation.

## Dependencies and user-added ingress

`oberth install` continues installing upstream Argo Workflows and optionally the
bundled secret store. v0.22.0 removes the 100m/128Mi requests and control-plane
priority class that Oberth installed on those two dependencies; their upstream
chart defaults apply. Stored dependency values are preserved, including operator scheduling choices.
The installer no longer strips former defaults or `controller.namespaceParallelism`.

Namespace/RBAC, executor image/profile and security settings needed by Oberth
remain. The installer sets Argo's global run cap and preserves its existing
default. It adds no new dependency scheduling policy. Operators own scheduling
choices for user-added ingress and other cluster components.

See [where limits live](where-limits-live.md) for native controls and
[release notes](releases/v0.22.0.md) for the source-delivery deviation and upgrade
actions.
