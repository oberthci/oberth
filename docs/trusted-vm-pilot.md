# Trusted VM pilot foundation

Required-suite dispatch remains unavailable. `.oberth/tests.yaml` retains its
closed version-1 offline eBPF profile; a requirement or unreadable policy fails
before build creation and publication, including recovery and fast-forward
promotion. The Beacon types and journal described here are inactive host-side
foundations. They add no repository-selected command, image, URL, credential
or execution option.

## Bound plan and artifact custody

`vmrunner.PilotPlan` describes the separate `beacon-transport-amd64-v2` subset:
one CPU, 512MiB, a RAM-root guest and at most ten minutes. It binds the exact
candidate and independently resolved suite revision, guest/conductor image
references, kernel/initramfs/helper digests and captured Beacon artifact.
Digest syntax validation does not verify producer signatures.
The retired version-1 transport profile is refused; its sealed plans cannot be
reinterpreted using the version-2 artifact path.

Artifact capture accepts only `bin/beacon`, a regular single-link
file from 1 byte through 64 MiB. Linux resolution uses `openat2` beneath the supplied
root with symlinks and magic links forbidden. Two bounded hash passes and
metadata checks reject changed content; retained bytes stay in host memory.
A read-only reader over the retained memory is available only for the matching
sealed plan; the capture file descriptor is already closed. Unsupported
platforms refuse capture. This does not establish a signed producer identity
or a guest-delivery transport.

## Durable ownership and capacity

Schema 13 retains the offline `vm_executions` lifecycle journal. Additive
schema 14 introduces `vm_suite_executions`, `vm_suite_resources` and the single
`vm_capacity_slots` authority shared by both paths. Existing pending lifecycle
work retains capacity during migration. The normal schema build/snapshot
process applies these migrations; the live daemon does not silently migrate
an older database.

The pilot journal reserves an immutable plan for its exact uncredentialed,
nonrelease run. Each closed resource intent is durable before create, then
binds its observed API UID. An ambiguous submitted create retains its cleanup
obligation; a not-found observation alone cannot erase that uncertainty.
Credential classification and VM/pilot submission check both orderings in
their serialized transactions, preventing credentials from being introduced
while submitted work remains uncleaned. VM/pilot journal mutations append
audit records; credential classification has no separate audit append.

The cleanup interface accepts host observations and UID-precondition deletion.
It discovers and retains late owned children, including when a parent has
already disappeared. A delete acknowledgment does not prove absence. Bounded
cleanup uses a separate context after cancellation; recovery cleans resources
and cannot manufacture a passing result. Capacity remains held until all
durable obligations are discharged.

## External conductor and one restart

The independently owned E2E conductor defines eight ordered cases:
authenticated relay, wrong tenant key, invalid token, replayed token, missing
v2 tenant, near-match tenant, near-match domain and fresh relay after restart.
The fixed inventory is bound to the suite revision. Canonical bounded stdout
frames carry ordered case progress and a final completion event. A guest log,
VM phase or Job condition cannot supply conductor process identity or exit.

Host evidence must bind one actual Job/Pod/container attempt, image digest,
spec identity and start time. A second Pod/container attempt poisons the
result. A complete result stream remains provisional until the same actual
process has exit code 0, signal 0, restart count 0, reason `Completed` and a finish
time within the admitted deadline.

The last case can request one explicit `restart-beacon` operation. Case-start
progress alone grants no restart authority. The journal claims the canonical
request once before replacement submission. Old VMI and owned launcher
absence must be observed before the new attempt, with distinct VMI/launcher
UIDs, a fresh TLS leaf pin and the same per-run fixture generation. The exact
reply is persisted for identical replay; a changed acknowledgment is refused.

`VerifyPilotPublication` accepts only the sealed plan and a protected receipt
source. The store returns a completed receipt only after the exact attempt,
case inventory, endpoints, restart and complete resource cleanup agree. This
interface is intentionally disconnected from scheduler capability.

## Work required before activation

The source foundation does not provide independently verified producer
artifacts, a concrete closed networked guest runtime, actual admitted
guest/conductor spec and same-node isolation proof, authenticated attach,
memory-only guest key/artifact delivery, or the coordinator joining those
parts to the journal. Synthetic fixture CA signing keys and result authority
must stay outside the guest; real host/repository/release credentials cannot
enter either workload. Transport abstractions alone prove none of this.

Activation requires those implementations, independent review, full local and
native CI gates, and live negative tests for credential isolation, process
replacement, restart and complete cleanup. The transport subset also leaves
historical Helm/HA/systemd/FUSE/eBPF and architecture coverage outstanding.
An inactive source review or a local TLS fixture pass cannot close that scope.
