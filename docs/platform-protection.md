# Platform protection

Oberth's own server Deployment keeps the shared control-plane PriorityClass
(value 1,000,000, `PreemptLowerPriority`). Both chart classes have
`globalDefault: false`; unrelated Pods do not inherit them. The server class
can preempt lower-priority Pods where Kubernetes permits it. It is not a
capacity guarantee.

New user Workflows receive no Oberth Pod priority class or preemption policy.
Repository Pod priority classes and arbitrary Pod patches remain refused;
authors can declare native Argo `spec.priority` for Workflow ordering.

The pipeline class (value 0, `Never`) remains on source helper Pods and executor
identity holder Pods. Its object remains in this release because v0.21.10
Workflows still running during upgrade reference it when they create later
Pods. It is a removal candidate after those Workflows end. These helper
assignments are documented in [native scheduling](native-scheduling.md).

## Dependencies and user-added ingress

`oberth install` continues installing upstream Argo Workflows and optionally the
bundled secret store. v0.22.0 removes the 100m/128Mi requests and control-plane
priority class that Oberth installed on those two dependencies; their upstream
chart defaults apply. On upgrade the managed overrides are removed. A
secret-store StatefulSet with `OnDelete` update strategy takes the change only
when its Pod is next recreated.

Namespace/RBAC, executor image/profile and security settings needed by Oberth
remain. The installer sets Argo's global run cap and preserves its existing
default. It adds no new dependency scheduling policy. Operators own scheduling
choices for user-added ingress and other cluster components.

See [where limits live](where-limits-live.md) for native controls and
[release notes](releases/v0.22.0.md) for the source-delivery deviation and upgrade
actions.
