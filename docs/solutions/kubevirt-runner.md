# Trusted Isolated KubeVirt Runner

Design for adding an operator-owned VM runner/controller that provides
bounded, least-privilege KubeVirt-based execution for workload integration
test suites (Oberth issue #278, blocking epic #277).

## Problem

Oberth's execution model creates Kubernetes Jobs in a pipeline namespace where
every container runs under a server-assigned security baseline. This model is
correct for building and releasing software but cannot host workloads that
require real VM isolation: Workload integration suites need k3s,
systemd, FUSE, eBPF, and PostgreSQL (which refuses root) inside a guest OS
that carries `/dev/kvm`. Five structural barriers in the current model block
this:

| Barrier | Location | Why it exists |
|---------|----------|---------------|
| Resource templates rejected | `pkg/argoworkflow/admit.go:324` | A resource template creates arbitrary Kubernetes objects using the workflow's own ServiceAccount -- on the release tier that is a direct privilege escalation |
| Repository security contexts rejected | `pkg/argoworkflow/admit.go:620` | The server forces the entire container baseline; a repository-authored context is either a lie or a weakening |
| UID 0 with capabilities dropped | `internal/argojob/spec.go:908` | PostgreSQL initdb and systemd refuse root; the baseline cannot be relaxed per-step |
| No Kubernetes token for branch steps | `internal/argojob/spec.go` | Branch-tier pods run `automountServiceAccountToken: false`; they cannot create KubeVirt objects |
| No production VM lifecycle wiring | `internal/vmrunner` | The lifecycle package has a concrete offline transport and durable journal; required-suite policy is enforced, but protected results and VM execution are not activated |

Historical issues #201/#202 were closed because the old canary dispatch was
unreachable, not because the underlying defects were fixed. Reusing old
templates verbatim is rejected by admission.

### What KubeVirt presence alone does not establish

Having KubeVirt deployed and `/dev/kvm` available on the node is necessary
infrastructure but establishes none of the following:

1. **No candidate-SHA binding:** nothing ties the guest workload to the exact
   source revision being tested.
2. **No suite revision pinning:** a candidate could modify the tests that
   evaluate it.
3. **No guest image pinning:** a mutable image tag can drift between the
   admission decision and the VM boot.
4. **No lifecycle bounds:** no bounded deadline, no automatic cleanup, no
   operator-owned cancellation.
5. **No trusted evidence:** an exit code printed as a log marker by code
   running inside the VM is as trustworthy as the VM's own integrity, which
   is to say: not at all.
6. **No credential isolation:** a pipeline container that can create arbitrary
   VMIs can label them to match any network policy, mount any Secret the
   service account can read, and escape the pipeline sandbox entirely.

## Design: operator-owned VM runner

### Architecture

The VM runner is a server-side controller that manages KubeVirt
VirtualMachineInstance (VMI) objects on behalf of pipeline steps. The pipeline
declares what to test; the server decides how to run it.

```
Pipeline step            Oberth server               KubeVirt
  declares               (operator-owned)
  VM run spec
      |                        |
      +-- admission --------->-+
      |   (validate spec)      |
      |                        +-- Create VMI -------->
      |                        |   (pinned image,      |
      |                        |    no SA token,        |
      |                        |    resource bounds)    |
      |                        |                        |
      |                        +-- Poll VMI phase ---->-+
      |                        |   (bounded deadline)   |
      |                        |                        |
      |                        +<-- VMI Succeeded/Failed
      |                        |
      +<-- VMRunResult --------+
          (lifecycle state, unknown process exit,
           UID-bound evidence and cleanup outcome)
```

### Trust model

The trust boundary runs between the pipeline namespace (untrusted) and the
server process (operator-owned). Three invariants:

**I1: The candidate cannot modify the test suite.** The suite revision is
bound at admission time and resolved by the server from a trusted source (the
Oberth git cache, not the candidate's checkout). The controller validates syntax and hashes the admitted spec, but that hash
does not independently authorize a suite revision. Production admission must
resolve the required suite and profile from a fresh trusted source and persist
them before execution; the candidate cannot supply this authority.

**I2: Lifecycle evidence and suite results are distinct.** KubeVirt's
`Succeeded` phase means voluntary shutdown and contains no test process exit
status ([KubeVirt API](https://github.com/kubevirt/api/blob/v1.9.0/core/v1/types.go)).
The controller therefore reports `ExitCodeUnknown` for either terminal phase.
A passing suite additionally requires the exact expected test inventory and
process exit from a pinned trusted supervisor, bound to the admitted inputs
and owned VMI UID. Candidate output, log markers, and phase alone cannot
satisfy that requirement.

**I3: The guest has no cluster access.** The VMI runs with no service account
token, no access to the Kubernetes API, and no network path to Oberth's own
namespace. A hypervisor or host-kernel compromise remains outside this isolation
guarantee. Network isolation and token exclusion do not make such a compromise
harmless.

### VM run specification

A pipeline declares a VM-backed step through a typed specification:

```go
type VMRunSpec struct {
    // RunID links this VM execution to its parent Oberth run.
    RunID string

    // Repo is the repository under test.
    Repo string

    // CandidateSHA is the exact source commit whose artifact is under test.
    // 40-character lowercase hex, validated at admission.
    CandidateSHA string

    // SuiteRevision is the test suite's pinned commit, resolved independently
    // of the candidate's checkout. This is the defence against a candidate
    // modifying the tests that evaluate it.
    SuiteRevision string

    // GuestImageRef is the VM's boot image, which must be an OCI reference
    // pinned by digest (sha256:...). A tag-only reference is rejected at
    // admission because it can drift between the decision and the boot.
    GuestImageRef string

    // Resources caps the VM's allocation.
    Resources VMResources

    // Deadline is the maximum wall time. Positive, bounded by the server's
    // configured ceiling.
    Deadline time.Duration
}
```

### Resource bounds

```go
type VMResources struct {
    CPUCores  int    // 1..MaxVMCPUCores (default ceiling: 4)
    MemoryMiB int    // 512..MaxVMMemoryMiB (default ceiling: 16384)
    DiskGiB   int    // 0..MaxVMDiskGiB (default ceiling: 64)
}
```

### Admission validation

Every field is validated before a VMI exists:

| Field | Rule | Why |
|-------|------|-----|
| CandidateSHA | 40-char lowercase hex | Prevents injection, ensures exact commit binding |
| SuiteRevision | 40-char lowercase hex | Same; resolved from trusted source |
| GuestImageRef | Valid OCI reference with `@sha256:` digest | Tag-only references drift; digest pins the exact bytes |
| CPUCores | 1..ceiling | Prevents host exhaustion |
| MemoryMiB | 512..ceiling | KubeVirt minimum + ceiling |
| DiskGiB | 0..ceiling | Bounded ephemeral storage |
| Deadline | 1m..ceiling | Prevents unbounded occupation |
| RunID | Non-empty | Links evidence to the parent run |
| Repo | Non-empty | Scopes the execution |

### VMI construction (server-side only)

The server constructs the VMI with these fixed properties:

- **Namespace:** The pipeline namespace (same as Argo workflows), never the
  server namespace.
- **Name:** Derived deterministically from the run ID (collision-proof, same
  pattern as Workflow names).
- **Labels:** `oberth.ci/run-id`, `oberth.ci/repo`, `oberth.ci/trigger`,
  `oberth.ci/tier=vm-runner`.
- **No ServiceAccount token:** no VMI service-account volume; fixed guest
  ServiceAccount. Observed launcher Pods must explicitly disable token
  automount and contain no Secret or projected token sources.
- **No host access:** No `hostNetwork`, `hostPID`, `hostIPC`, `hostPath`.
- **Resource bounds:** CPU and memory from the validated spec.
- **Deadline:** VMI's `terminationGracePeriodSeconds` plus a server-side
  context deadline.
- **Guest image:** Fixed amd64 `kernelBoot` uses `/boot/vmlinuz` and
  `/boot/initramfs` from the admitted digest. The guest has a RAM root, no NIC,
  vsock or host volumes; optional `emptyDisk` capacity follows the admitted
  bound. This recipe does not yet deliver candidate artifacts or run a suite.

### Evidence collection

The controller collects lifecycle evidence; this is insufficient for a
passing suite receipt:

1. **VMI phase:** `Succeeded` or `Failed`, read from the KubeVirt API. Neither
   phase is a guest process exit code.
2. **VMI metadata:** exact UID and creation timestamp retained from creation
   and checked on every observation. FinishedAt records the controller's
   observation time, not an API-provided guest process completion timestamp.
3. **Binding echo:** The candidate SHA, suite revision, and guest image digest
   are echoed from the spec that was admitted, not from anything the guest
   reports.

```go
type VMRunResult struct {
    Phase         string    // "Succeeded" or "Failed"
    ExitCode      int32     // Always ExitCodeUnknown (-1) for lifecycle evidence
    CleanupComplete bool   // Observed absence, not merely a deletion request
    Evidence      VMEvidence
    Duration      time.Duration
}

type VMEvidence struct {
    CandidateSHA     string
    SuiteRevision    string
    GuestImageRef    string
    VMInstanceName   string
    VMInstanceUID    string
    StartedAt        time.Time
    FinishedAt       time.Time
    ExitCodeSource   string // "unavailable": a separate trusted suite receipt is required
}
```

### Lifecycle management

```
Create  ->  Running  ->  Succeeded/Failed  ->  Cleanup
  |            |               |
  |            +-- deadline -->-+ (server cancels)
  |                            |
  +-- context cancel -------->-+ (server deletes VMI)
```

**Cleanup contract:** After any observed terminal state, cancellation or
observation error, the controller attempts UID-bound deletion in a separate
`context.WithoutCancel` timeout. Cleanup errors remain errors, set
`CleanupComplete=false`, and must retain durable cleanup and capacity
obligations. Deletion success requires observed absence of the VMI and its
owned launchers. API or cleanup failure can leave a resource running; the
scheduler must persist and reconcile that obligation. An ambiguous create
request cannot be discharged by a subsequent absence check because the
request may still complete. The execution deadline starts at resource
creation and is not extended by a resumed wait.

### Adversarial scenarios and defences

| Attack | Defence | Test |
|--------|---------|------|
| Caller changes the admitted suite | Immutable journal and observation reject changed identity; trusted upstream selection remains a separate gate | `TestVMJournalAdmissionAndIdentityCannotChange` |
| Guest prints fake exit code markers | VMI lifecycle always reports an unknown process exit; separate trusted receipt required | `TestWaitDoesNotInferProcessSuccessFromVMIPhase` |
| Candidate removes published tests or substitutes local main | Fresh private upstream policy retains required tests; absent protected executor refuses execution | `TestDefaultPolicyIgnoresLocallyPushedMain`; `TestTrustedPolicyRefusesBuildWithoutProtectedExecutor` |
| Green build or recovered publication bypasses required tests | Mandatory publication policy check includes recovered outbox and fast-forward promotion | `TestTrustedPolicyRechecksBeforePublication`; `TestTrustedPolicyPublicationRecoveryAndFastForwardRefuseMissingEvidence` |
| Admission injects launcher credentials | Actual launcher rejects token automount, wrong account, Secret and projected-token sources | `TestKubeVirtRejectsLauncherCredentialInjection` |
| Caller selects server namespace or guest networking | Backend requires distinct namespaces and fixes an offline guest spec | `TestKubeVirtClosedGuestSpec`; live network isolation still requires validation |
| Candidate uses tag-only guest image | Admission rejects references without `@sha256:` digest | `TestVMRunSpec_RejectsTagOnlyImage` |
| Candidate requests unbounded resources | Admission enforces ceilings on CPU, memory, disk, deadline | `TestVMRunSpec_RejectsExcessiveResources` |
| Race between admission and VMI creation | Server constructs VMI from the admitted spec, not from a re-read | Structural: no second read path |
| Owner restarts after an ambiguous create | Durable intent retains capacity; only exact UID cleanup or definite refusal can settle it | `TestVMJournalSurvivesOwnerRestart` |

### Nonroot execution (PostgreSQL lane)

PostgreSQL's initdb refuses UID 0. The existing nonroot execution profile
(`nonroot-static-v1`) already provides a verified UID 65534 lane for
container/script leaves. For VM-based PostgreSQL:

- PostgreSQL must run as an appropriate nonroot user inside the guest. Guest
  users belong to the guest kernel; they are not a direct mapping to host
  user IDs. The KubeVirt launcher has its own independently enforced host
  security context.
- No change to Oberth's container security baseline is needed: the VMI is not
  a container step, and its internal UID mapping is the guest OS's concern.

For non-VM PostgreSQL in ordinary container steps, the existing nonroot
profile applies. This is independent of the VM runner.

### ARM execution

The concrete backend currently selects amd64 nodes and an amd64 kernelBoot
recipe. Native ARM coverage is not implemented. A digest pins bytes but does
not itself prove their architecture; an ARM profile must verify the image's
platform and kernel identities and run on matching native capacity.

### Integration with existing Oberth components

The lifecycle transport and journal are not yet a production suite runner.
The scheduler enforces required-suite policy before creating a build and
immediately before publication, including restart recovery and fast-forward
promotion. Required profiles fail with an explicit unavailable-executor error;
an ordinary green build cannot substitute for their missing trusted results.
Candidate-controlled annotations cannot authorize a profile, suite revision,
image or credential.
The closed `.oberth/tests.yaml` contract names a supported profile and artifact
only; its required upstream baseline must be freshly read from private
upstream tracking refs, so a branch cannot remove it through a local main push.
Unreadable policy or an unavailable upstream fails closed. Tag policy uses the
tested commit, not the annotated tag object. No profile is activated by these
source changes.

Future integration must report trusted suite steps through existing run
progress/log/MCP surfaces and reconcile the journal before admitting new VM
work. A cleaned lifecycle record carries no passing suite result. The old
pinned guest harness is not sufficient authority for arbitrary candidate eBPF:
helpers such as override-return or signal can affect processes in that guest.
The result supervisor must remain outside candidate control, with exact test
inventory and a real process exit bound to the admitted inputs and VM UID.

### Dependencies

- **KubeVirt CRDs:** Must be installed in the cluster. The controller uses
  the existing dynamic Kubernetes client for `kubevirt.io/v1` VMIs.
- **Virtualization capability:** Activation must verify the installed KubeVirt
  configuration and node hardware capability. Emulated local testing is not
  native execution evidence.
- **Network policy:** The launcher requires a separately reviewed least-privilege
  network policy. The current guest has no NIC. Networked suite profiles and
  their egress authority remain separate work.

### Implementation phases

**Phase 1 (integrated baseline, with lifecycle hardening):**
- Design document (this file)
- Core types: `VMRunSpec`, `VMRunResult`, `VMEvidence`, `VMResources`
- Admission validation with complete bounds checking
- Trust binding: immutable spec identity, candidate/suite/image pinning
- Controller skeleton: interface and lifecycle state machine
- Adversarial negative tests: all scenarios from the table above

**Phase 2 (source and isolated API tests; not live activation):**
- Concrete closed offline KubeVirt transport and observed launcher checks
- UID-preconditioned cleanup with observed VMI and launcher absence
- Audited durable reservation, create intent, UID binding and restart recovery
- Required-suite policy before build and publication, including recovery
- Console/result supervisor, artifact delivery and live validation remain pending

**Phase 3:**
- Scheduler execution with independently authorized, activated suite profiles
- Run progress integration (progress markers from VM runs)
- End-to-end test with a real KubeVirt workload

**Phase 4:**
- ARM node support
- Verified immutable guest/kernel/initramfs/helper packaging
- PostgreSQL integration test suite
- CTTV full-stack validation

### Success criteria

1. Every VMRunSpec field is validated at admission before any VMI exists
2. A candidate cannot modify the test suite that evaluates it
3. Lifecycle identity comes from the API; suite success requires a protected
   trusted supervisor process and complete expected test inventory
4. The guest has no access to cluster credentials or the server namespace
5. Resource bounds are enforced and prevent host exhaustion
6. Cleanup attempts are bounded; unresolved resources retain durable
   obligations and capacity and cannot contribute passing evidence
7. All adversarial tests pass and cover the threat table above

### Non-goals

- Replacing the Argo workflow engine for ordinary build/release steps
- Running an eBPF kernel-variant matrix (that is the QEMU/virtme-ng
  path, see `docs/solutions/kernel-e2e-bridge.md`)
- Multi-tenant VM scheduling across nodes
- Persistent VM instances (every VMI is ephemeral and run-scoped)
- GPU passthrough or SR-IOV

## References

- Oberth issue #278: This issue
- Oberth issue #277: Parent epic
- Oberth issues #201, #202: Historical canary dispatch (closed, not fixed)
- `docs/solutions/kernel-e2e-bridge.md`: QEMU/virtme-ng path for kernel testing
- `pkg/argoworkflow/admit.go`: Admission gate
- `internal/argojob/spec.go`: Container security baseline
- `internal/argojob/nonroot.go`: Nonroot execution profile (pattern reference)
- `internal/argojob/nonroot_verifier.go`: Trust verification model
