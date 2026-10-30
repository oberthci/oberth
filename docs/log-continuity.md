# Step log continuity and failed init containers

A step can fail before its main container starts. Oberth selects the current
failed init container in that case, rather than waiting for a nonexistent main
container identity. Its tail is attributed to the step, along with the observed
container name, exit code, termination reason and bounded termination message.
Successful identity-delivery init containers are not substituted for step logs.
Pod namespace, owner UID, node annotations and container identity must match.
A termination message is authorized run output, like the container log itself;
control characters are quoted so it cannot introduce forged step prefixes.
Structured diagnostics add `container_kind: init`, `message`, and
`message_truncated` when applicable; older main diagnostics remain readable.

Complete timestamped records are retained as they arrive. A bounded write-ahead
journal (`<run>.log.pods.jsonl`) stores the received attributed bytes and their
inclusive timestamp/count/digest cursor before the ordinary log projection is
written. Complete records are batched within a network read; a partial transport
record never becomes command output. On adoption, recovery repairs a missing or
partial final projection from those actual received bytes. It then resumes at the
durable boundary, preserving repeated identical lines at one timestamp. Finished
cursors do not replay finished Pods. Old runs without a cursor retain their
explicit adoption gap because arbitrary historical replay could duplicate output.

The journal is bounded to twice the configured run-log byte budget plus 8 MiB of
checkpoint overhead; a frame is bounded to 2 MiB including JSON/base64 encoding.
The normal run and per-step output limits still apply. Journals count toward log
retention usage while active. After the run's durable terminal verdict and a
synced, indexed log projection, the journal is pruned immediately. Active owner
resumptions keep it; a failed sync/index/cleanup preserves recovery evidence and
never makes cleanup a scheduler gate.

Corrupt or unopenable journals lose cursor authority for that run. Oberth writes
an explicit continuity gap, records a best-effort `run.log-continuity.lost` audit
observation, and continues observing the Workflow without restart cursors. A
retained loss marker prevents stale cursor reuse even if file removal failed.
Journal failure before projection falls back to writing the received body once;
a partial projection keeps its valid WAL for recovery rather than appending the
same bytes again. An unavailable retained-file handle cannot turn an observed
terminal Workflow into an API re-probe/delete failure: progress/termination
observation continues, unavailable output stays lost, and the audit/global warning
identifies the gap. Corrupt data is never trusted or guessed.

Each normal commit captures any intervening plain-log prefix in bounded WAL
frames, retaining the previous source cursor until the body frame. One WAL sync
protects the prefix, received bytes and new checkpoint before body projection.
The main log is synced once at terminal finalization before pruning. Recovery
uses checksummed received bytes to repair partial or zeroed projection ranges;
an unrecorded legacy prefix gap remains explicitly lost. The existing v1 wire
format is retained, including safe old-cursor prefix frames. A torn prefix batch
cannot advance a source cursor over an unprojected body.

Complete records are grouped within bounded 64 KiB scanner reads/batches; a lone
burst still commits durably, without a one-second asynchronous durability
window. The shared writer serializes cursor commits, output and progress
controls. Filesystem writes and syncs provide durability; network deadlines do
not cancel a kernel filesystem operation.

Final log reads allow 60 seconds per request and at most three attempts, with
short bounded backoff. A 120-second total drain budget also covers follower
overflow batches. Log-read failures identify a bounded class (for example
`pod-not-found`, `api-throttled`, or `container-identity-unavailable`) without
copying backend response bodies into public errors. Previously retained bytes
survive these failures. A missing container exit code or termination message is
explicitly unavailable. A real Argo node exit code can be shown with its source;
Oberth's fallback failed-step code is never described as an observed container
exit code.

Oberth disables submitted Workflow PodGC and uses Workflow owner-reference
collection at its configured completion TTL, with a minimum TTL of 180 seconds.
This leaves room for settling, diagnostics and the shared final-drain budget;
longer configured TTLs are preserved. There is no indefinite finalizer. An
operator's manual deletion, external PodGC or kubelet rotation can still remove
historical bytes; the collector reports that gap instead of manufacturing a tail.

The supplied docs cases (2a1971bf and 105ea601) were init failures: `install-jq`
exited 23 while main was Waiting/PodInitializing, so the old main-only identity
check never reached the log endpoint. Their supplied status does not show Pod
GC or a terminated main container. The separate init-readonly fix addresses the
write refusal. The port case 82921a70 was already garbage-collected before dumps
were collected; its original final-read error class cannot be established from
the available evidence.
