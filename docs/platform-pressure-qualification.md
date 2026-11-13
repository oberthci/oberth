# Qualifying platform recovery under pressure

The server retains its control-plane priority class. Argo and OpenBao have no\ninjected requests or priority class; operators configure their cluster policy.
Operators must qualify the actual target cluster before relying on it for
recovery. An isolated fixture can exercise scheduling and Pod recreation, but
pause-container results do not establish application health or recovery.

Use a disposable cluster to verify Kubernetes behavior under memory and disk pressure,
configured PriorityClasses and tolerations, recreation with new Pod UIDs,
and recovery after pressure clears. Preserve sanitized effective kubelet
configuration, node conditions, Pod priority, QoS and readiness as evidence.
Never induce pressure on a production host for this check.

Then qualify the target installation's resource headroom, startup, OpenBao
unseal, database access and ingress recovery through a separately authorized
rehearsal. Include storage-limit eviction behavior and provider-specific
scheduling policy. External workloads and controllers require their own
protection and recovery policy outside Oberth.

The repository's `hack/qualify-platform-pressure.sh` is an optional fixture;
its output is evidence only for the environment and cases actually exercised.
Store installation-specific measurements and execution logs in operator records.
