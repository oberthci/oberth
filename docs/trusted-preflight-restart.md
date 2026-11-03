# Trusted preflight restart recovery (#1172 slice 3)

Design decision: do not reattach a trusted conductor after owner restart. The
sealed pilot journal spends one attach right before opening the protected channel
(`store.ClaimConductorAttach`); ambiguous attachment cannot renew that right.
`ReadConductorStream` keeps its validated result sequence and control-operation
progress in memory. There is no protocol for an attested resume cursor. Reopening
exec or treating saved Pod output as a continuation would weaken those bindings.
Workflow-backed trusted preflights retain their cleanup-only recovery contract,
rather than gaining a new path for producing recovered positive evidence. This
slice implements durable interruption and one fresh preflight attempt.

A parent with a reserved KVM preflight and no main Job is left for trusted
recovery, rather than the ordinary empty-Job branch requeue. Owner shutdown also
leaves that parent recoverable. Recovery retains the exact journaled Workflow
UID and uses the existing spec/ownership checks and observed-absence cleanup.
An ambiguous create must still prove absence over the configured request horizon,
or recover its exact admitted identity; a missing workspace or failed identity
check cannot authorize cleanup of an unrelated object. Recovery of each pending
execution is bounded as a whole to two minutes (or the caller's earlier
deadline): the ambiguous-create absence window (60 seconds plus 30 seconds of
observation slack), identity reconciliation, the negative refusal and the exact
cleanup all share that budget. Uncertain or timed-out cleanup keeps its journal,
capacity reservation and parent pending. Recovery runs once, at owner startup,
before claims resume; there is no background retry. A parent whose cleanup times
out therefore keeps its KVM slot, stays undelivered and cannot be retried until
the next owner restart repeats recovery (fail-closed).

An unreceipted execution receives the existing negative `canceled` refusal before
cleanup. Existing positive and negative receipts are immutable. Even a Workflow
that completed while the server was down cannot supply new positive evidence
through recovery. A previously persisted positive receipt stays on its old run;
the negative `kvm.restart-interrupted` audit records the undelivered parent's
interruption without rewriting that receipt.

After cleanup, interruption, issue projection, a fresh queued run, its predecessor
link and `kvm.restart-retry` audit commit in one transaction. The retry retains the
repository, actor, ref, object/tested/base SHAs, trigger and admission sequence.
It acquires admission anew, reloads approved policy and credentials, and earns a
new intent and receipt before any main Workflow or publication. CI credential
admission is recomputed; release classification remains bound to the original
release request. Old receipt authority never transfers to the new run ID.

The automatic retry budget is keyed to the persisted admission sequence, which
ordinary restart re-fires inherit. A restart before the replacement reserves its
VM therefore cannot reset the budget. Another interruption of that lineage is
terminal, with a negative recovery audit and no automatic replacement. An
operator's explicit new attempt has its normal separate admission identity.
Cancellation, supersession, newer work on the ref, archived repositories and
already-recorded suite/infrastructure refusals prevent automatic retry. A main
Job or publication on the parent refuses this recovery path entirely. Sealed
plan/apply pilot execution retains its existing fail-closed cleanup policy and
is never replayed through a CI retry.

A promotion gets a new immutable promotion record linked to its predecessor.
This is a retry of its exact preflight-tested plan, not a new merge: source,
previous target head and planned result remain unchanged. The admitting actor
must still have live, consistently scoped authority, and a grant must remain
unrevoked, unexpired and unconsumed. The normal publication ledger and target-head
comparison remain in force: if main moved, the retry cannot push that old plan.
Tag/release retries also retain the existing unpublished-tag guard. No schema
change or new stored column is needed; retry lineage lives in the audit chain.
