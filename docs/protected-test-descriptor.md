# Protected test policy and descriptors

Protected source policy lives in `internal/protectedtests`. The existing VM
adapter forwards its contract API to that package, so execution, journal records,
recovery and publication continue to use the same contract and recipe bytes.
The default runtime remains the legacy adapter. The explicit generic capability
route below is preparation requiring separate external qualification.

A descriptor maps a valid v2 `.oberth/tests.yaml` contract to the complete
operator recipe: suite archive SHA256, guest archive SHA256, host harness image
digest and credential-free HTTPS endpoints. It also retains the exact profile
and artifact list. Descriptor fields are private, and artifact slices are copied
on input and output. An empty descriptor cannot verify inputs or match policy.

`SelectDescriptor` selects only an exact suite/guest pair from the current,
bounded, unambiguous operator allowlist. It runs before any suite fetch.
`BindContract` reconstructs the same mapping from a stored intent, but does not
grant approval: cleanup must remain possible after recipe revocation.
Publication separately requires an exact current contract match and approval of
every stored recipe field, including the host image and both endpoints.
Removing a recipe or changing just its harness or endpoint refuses the old
execution. Existing fresh upstream baseline checks still reject candidate
removal and unauthorized rotation; a descriptor does not authorize deleting the
old contract. Approved paired pin rotation still uses the current operator
recipe and existing mandatory audit checks.

The complete digest-verified suite bundle supplies the trusted input closure.
Every archive byte is hashed before its manifest is interpreted. Existing bounds,
canonical USTAR regular-file restrictions, manifest checks and private all-case
inventory remain enforced. Files inside that bundle cannot be changed by a
candidate while retaining the approved digest. No candidate-selected source tree
or ordinary workflow digest becomes an independent trust root. The descriptor
itself does not prove the meaning of tests or success of execution.

The existing protected harness still verifies the guest bundle and snapshots
only bounded candidate artifact data. It owns execution and the verdict. The
server still validates actual Workflow/Pod/container/image identities, exit and
deadline, binds the report to the immutable intent and full case inventory, and
requires durable cleanup before release delivery or publication. The descriptor
is derived from the existing intent; it adds no fields to serialized intents,
receipts or their digests. Recovery, UID fencing, cancellation and audit refusal
remain mandatory.

Generic execution and protected cross-Workflow evidence handoff require
separate source review and actual runtime qualification. This preparation does
not qualify an external executor or permit execution-engine retirement. Install
and qualify preparation before a separate removal release; retain the current
recovery path and historical evidence throughout.

## Execution evidence and handoff preparation

The protected package also owns execution intent, host report, process binding,
receipt and refusal validation. VM names remain aliases and forwarding calls.
Historical JSON field order, tags, intent/report versions and digests remain
unchanged; existing journal rows and audit records are not rewritten. The
current journal and its capacity, restart, cancellation and exact UID cleanup
transitions remain authoritative. No new table or migration is needed for this
shared verifier.

`Execution.ValidateHandoff` is the common delivery and publication check. It
loads no authority from candidate output: its caller supplies the current parent
run/repository, exact candidate and baseline, fresh protected contract and current
operator recipe approval. It requires submitted, successful, cleaned evidence
with the same Workflow UID and a complete valid process/host receipt. It refuses
missing or changed policy, revoked recipes, another run or repository, missing
cleanup, refusal, substituted identity, failed/restarted process, deadline or
inventory mismatch. A generic capability grant cannot substitute for this check.
The scheduler invokes it both before the main Workflow receives credentials and
at publication, using the existing durable journal each time. Its result is not
a reusable bearer token or permission to skip current policy/audit checks.

## Explicit generic capability runtime

`--trusted-tests-runtime=generic-capability` (Helm `trustedTests.runtime`)
selects the generic route. The default is `legacy`; an existing capability grant
never switches the executor. Approved recipes and a current per-repository
capability naming the exact namespace and `devices.kubevirt.io/kvm` are required.
The complete emitted resource graph must fit that grant. This route does not
require `--vm-kvm-enabled` and refuses candidate builtin-KVM injection.

The independently pinned harness receives exactly the existing intent bytes.
A separate immutable server admission records the complete capability and the
explicit positive supplemental group. A forward migration binds that admission
to the existing protected journal and capacity reservation atomically with its
chained audit action. Audit-state and mutation-fence verification compare the
projection with the chained event. Missing or corrupt metadata refuses instead
of selecting legacy execution; historical intents and audit rows are preserved.

Admission, Create, receipt acceptance and credentialed delivery/publication
recheck current authority. Changed or revoked grants refuse. Cleanup alone may
reconstruct the original immutable grant and group after restart, including an
uncertain Create with no recorded UID. It still requires exact owned Workflow
reconstruction and UID-fenced cleanup. A zero current group permits generic
startup recovery only: new admission, Create and Receipt refuse. Reconfiguration
does not rewrite the original admission or recreate a revoked grant.

The generic route retains the same closed host leaf, credential isolation,
full Pod/process observer, all-case verifier, cancellation and cleanup journal.
A resource grant or successful resource status cannot supply a protected test
verdict. External runtime qualification remains required before activation and
before a separate legacy engine removal release.
