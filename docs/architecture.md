# Architecture

Oberth is a Git ingress, CI record and publication service. Kubernetes places
Pods and Argo executes Workflows. The [native scheduling](native-scheduling.md)
page describes authored resources, placement, synchronization and deadlines.

## Git ingress and run records

An authenticated SSH connection binds one public-key fingerprint to an actor.
Git pushes update a bare repository cache on the persistent data volume. Before
publishing refs, the receive path records an actor-bound, fsync-backed outbox
reservation. Callbacks replay accepted branch and tag events idempotently into
SQLite after failures or restarts. Repository locks preserve receive order;
network publication happens outside those locks.

Branch pushes resolve `.oberth/build.yaml`; tag runs resolve
`.oberth/release.yaml`. A run retains the exact commit, actor and trigger.
A newer push can supersede an older run on the same branch. The scheduler
claims eligible records in acceptance order and starts their submission work.
Archived repositories, unfinished cancellations and pending publication on the
same ref can delay a claim. Oberth has no capacity scheduler, run ceiling,
priority queue, resource reservation or queue-position field.

The cron service resolves scheduled work from the current default-branch
schedule and enqueues ordinary runs. Promotion work has its own durable order
for Git merge and publication. These are CI lifecycle policies; Argo controls
Workflow parallelism and Kubernetes controls Pod scheduling.

## Source resolution and Workflow submission

The source resolver pins the exact source and fragment set in a source receipt.
Submission checks that binding again. Admission validates dangerous content and
credential boundaries while preserving admitted native Argo fields. Trusted
upstream policy refresh can delay submission; it counts no capacity and rewrites
no requests.

The server creates a per-run source claim in the pipeline namespace and fills
it with the resolved checkout through a short-lived seed Pod. Containers receive
the source read-only. Configured repository/tier caches are separate from that
checkout. Artifact collection uses a source helper after execution. See
[pinned sources](pinned-sources.md) and [native scheduling](native-scheduling.md)
for the current delivery and helper behavior.

Branch execution uses its CI identity. Credentialed runs use installer-provisioned
identities and explicit grants. The server delivers short-lived, Pod-bound
executor tokens to the Argo helpers. Secret-bearing steps use
`oberth secretstore exec`; arbitrary Secret access and service identity
overrides remain refused. See [secret delivery](argo-secret-delivery.md).

## Observation, logs and recovery

Argo Workflow and Pod observations drive run and step results. Step progress
markers and the retained run log keep step status and named log slices
available after Kubernetes collects the Workflow. A Pod-log continuity journal
supports resumed following and final log collection. Display rows come from
`StepResult`; retained Workflow/step timing and termination diagnostics remain
available through full MCP run details. Admission timing episodes are absent.
The dashboard uses the compact
projection and does not load that timing record.

Run observations explain native parallelism, lock and Pending-Pod waits using
existing statuses and reason lines. Authored deadlines and retry strategies
belong to Argo. Oberth cancellation and supersession delete the owned Workflow.
On restart, recovery verifies bindings before adopting existing Workflows.
Ambiguous creates have a durable cleanup obligation and a background settlement
task. Stranded runs can be requeued only under the existing execution-evidence
checks. See [restart handover](restart-handover.md).

## Promotion and publication

Promotion merges eligible sources with a freshly fetched target in a private
checkout and tests the candidate when required. Ordered promotion trains test
their final tree. A publication records the exact tested commit before an
upstream push; the push is never forced. Target movement and transport failures
follow the durable promotion/publication retry rules. Parked sources always
receive merged-tree CI. See [promotion trains](promotion-trains.md) and
[park provenance](park-promotion.md).

## Persistence, audit and service boundaries

SQLite stores repository/run history, step results, grants, publications and
audit records on the server volume. Forward-only migrations have a strict
sequence and ledger. Existing deployments preserve their schema identity,
audit domain and witness-key derivation context. Upgrade preflight checks
stored user values against the target chart schema. When moving to a chart
without Transit, the installer omits only its retired `secretstore.transit.enabled`,
`.mount` and `.key` leaves. Other unknown values are refused by the schema.
Installed compatibility identities remain in the initial stored-user snapshot,
below requested overlays; restore uses the restored archive's identities.

Mutations pass the audit integrity gate. The local hash chain is verified on
startup and during operation. Optional timestamp and Rekor anchoring add
external evidence and rollback-external continuity; local-only mode does not
detect a rollback to another internally consistent database. See
[audit configuration](security.md).

The server exposes Git/SSH, authenticated HTTP/MCP, the dashboard and bounded
read-only observations. Readiness requires completed recovery, the database and
configured upstream identity. Kubernetes and audit health remain status
diagnostics; mutation authorization still checks audit integrity. Shutdown
withdraws readiness before draining clients. See
[readiness under load](readyz-load-flap.md).
