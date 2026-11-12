# Infrastructure failures and retries

Oberth v0.22.0 does not automatically create another run after an infrastructure
failure. The failed run retains its observed cause. Manual `run_retry` and
`run_rerun` remain available with their existing cancellation, publication,
credential and ref-safety checks.

Authors choose native Argo retries on individual templates:

```yaml
retryStrategy:
  limit: 2
  retryPolicy: OnError
  backoff:
    duration: 30s
    factor: 2
```

`OnError` retries controller/executor errors; it does not retry an ordinary
nonzero command exit. Use the native policy appropriate to the step and make
any repeated external mutation safe. This repository uses `OnError` on its
ordinary leaves and retains the existing `OnFailure` policies on publishers.
Those credentialed steps deliberately avoid retrying executor errors such as
rejected credential setup. It does not retry whole DAGs.

Source delivery still precedes Workflow creation. A clone or source-seed
failure has no Workflow in which Argo could retry a template; use manual retry
after fixing the cause. See [native scheduling](native-scheduling.md) and
[v0.22.0 upgrade actions](releases/v0.22.0.md).
