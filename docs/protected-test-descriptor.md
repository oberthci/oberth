# Protected test policy and descriptors

Protected source policy lives in `internal/protectedtests`. The existing VM
adapter forwards its contract API to that package, so execution, journal records,
recovery and publication continue to use the same contract and recipe bytes.
This is preparation for a separate execution adapter; it introduces no new
configuration, grants, policy file, migration or execution route.

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

A subsequent generic adapter and protected cross-Workflow evidence handoff need
separate source review and actual runtime qualification. This extraction does
not qualify an external executor or permit execution-engine retirement. Install
and qualify preparation before a separate removal release; retain the current
recovery path and historical evidence throughout.
