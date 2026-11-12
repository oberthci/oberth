# Native Workflow priority

Declare Argo `spec.priority` in repository YAML. Argo uses it in its Workflow
queue; Pod priority classes remain outside repository authority. Oberth adds
neither run-kind priority nor a release mutex to a user Workflow.

```yaml
spec:
  priority: 100
  synchronization:
    mutexes:
      - name: repository-release-finalize
```

Use a shared native mutex around the release finalization that must be serial.
Oberth's own `.oberth/release.yaml` declares its release priority and mutexes in
Git. Publication custody and branch-delete/supersede cancellation remain Oberth
lifecycle controls. See [native scheduling](native-scheduling.md).
