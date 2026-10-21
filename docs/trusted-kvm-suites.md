# Trusted KVM suites (tests.yaml v2)

The server can require an operator-approved Linux/amd64 suite before any main
build or release delivery. The candidate's `tests.yaml` selects a closed recipe:

```yaml
version: 2
profile: kvm-suite-amd64-v1
suite_sha256: <64 lowercase hex characters>
guest_sha256: <64 lowercase hex characters>
artifacts: [build/program]
```

Published default-branch requirements survive candidate removal. Changing a
retained contract is rejected. V1 remains denied: this implementation does not
reinterpret legacy evidence as a trusted v2 receipt.

Enablement requires `--vm-kvm-enabled` and `--trusted-tests-recipes=FILE` (Helm:
`vm.kvm.enabled` and `trustedTests.recipes`). The JSON array contains public
operator policy: `suite_sha256`, `guest_sha256`, `harness_image` (complete OCI
`@sha256` reference to the Linux/amd64 platform manifest actually reported by
the kubelet runtime, not a multi-platform index), `suite_url`, and `guest_url`. Endpoints must be public HTTPS,
without user information, query credentials, fragments or redirects. Hashes
alone authenticate content, not its publisher: the independent operator
allowlist is authority to execute that exact suite driver. Admission refuses
unknown recipes before fetching or executing them. Revoking or changing the
recipe also invalidates old receipts at publication. No recipe is enabled by
default; no example digest is a production artifact.

## Producer contract and trust boundary

A suite is a complete uncompressed canonical USTAR bundle, maximum 64 MiB,
containing only unique regular files. `manifest.yaml` is strict YAML (16 KiB):

```yaml
version: 1
profile: kvm-suite-amd64-v1
guest_sha256: <complete guest bundle digest>
artifacts: [build/program]
cases: [kernel-probe, application-behavior]
```

There are 1–256 unique bounded case IDs. `suite-driver` is a fixed-path static
Linux/amd64 ELF executable from the same approved bundle. The manifest has no
command language. The harness executes that host driver once per manifest case:

```
suite-driver --case CASE --qmp HOST_PRIVATE_SOCKET \
  --guest-control HOST_PRIVATE_SOCKET --deadline RFC3339_TIMESTAMP
```

The driver is trusted code and owns the assertions, including assertions about
privileged eBPF, FUSE, systemd or k3s guest behavior. Exit zero from the host
process contributes one passing case. Driver stdout and guest console/probe
output do not contribute inventory or override a failed process. Guest responses
are untrusted inputs to explicit assertions; a driver that merely copies a
reported guest PASS is not a valid trusted suite. Operator review of the driver
is essential. The kernel, QEMU and approved driver are trusted substrate; this
model does not assume that arbitrary guest root code cannot forge guest output.

The complete guest bundle is independently pinned, bounded to 256 MiB and
verified before parsing. It contains exactly USTAR regular files `kernel` and
`initramfs`. The approved guest implements the suite's probe protocol over the
virtio serial port `org.oberth.probe`; the host driver connects to its private
Unix socket. Candidate artifacts arrive as a read-only tar byte stream through
QEMU fw_cfg `opt/oberth/artifacts`. Guest init can read that data via Linux
fw_cfg sysfs and stage it inside the guest. This channel carries data only.
No driver, result file, host directory or QMP socket is mounted into the guest.
QEMU is forced to KVM on Linux/amd64, two CPUs/2 GiB, no NIC, no default devices
and no user configuration. Its mandatory seccomp sandbox uses
`-sandbox on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny`.
Suite authors provide actual guest initialization
and host assertions; Oberth does not fabricate a passing generic guest.

## Execution, evidence and releases

Oberth runs producers from `.oberth/build.yaml` in a fresh credential-free preflight Workflow,
then appends the server-owned pinned `trusted-tests` leaf. Candidate secret,
WIF, hook, retry, daemon and template-default capabilities are refused there.
The trusted leaf receives only a read-only artifacts mount and private scratch.
After producers complete, the harness copies admitted regular files into private
bounded snapshots, hashing each path, size and byte content in contract order.
Symlinks, escapes and special files are rejected. Candidate bytes never execute
in the host harness.

To avoid repeating the full CI matrix, build.yaml may select a local producer
entrypoint for the preflight alone:

```yaml
metadata:
  annotations:
    oberth.ci/trusted-tests-entrypoint: suite-inputs
spec:
  entrypoint: ci
  templates:
  - name: suite-inputs
    dag:
      tasks:
      - name: compile
        template: compile-suite-inputs
      - name: stage
        template: stage-suite-inputs
        dependencies: [compile]
  # ci retains all normal checks, including the heavy matrix.
  # compile-suite-inputs and stage-suite-inputs are ordinary local templates.
```

The selected DAG/steps must produce every tests.yaml artifact under
`/work/artifacts`, including all dependencies needed to stage those files.
Missing artifacts fail the trusted harness; a template name supplies no evidence.
The complete blueprint is admitted under the same credential-free restrictions,
including templates outside the selected path. Empty, missing, templated and
server-reserved selections refuse. Without the annotation the preflight runs
the original entrypoint, preserving existing repositories.

The durable run's `{{workflow.parameters.oberth-trigger}}` is available to
producer guards for branch, promotion, schedule and tag runs. A declared
`oberth-trigger` input receives the stamped value rather than the document's
older-server default. The preflight keeps build.yaml's size-by-trigger admission
(and uses promotion sizing for a tag preflight), with at least M for the harness.
A producer required by the suite must still run on branches; guards must not
skip an artifact the retained suite contract requires.

Selection is part of the original build.yaml digest in the immutable intent,
bound to the same run, tested SHA and approved suite/guest/harness recipe. The
existing artifact snapshot digest and attested receipt remain mandatory. No
cross-run artifacts or mutable name-based cache are accepted. Repository adoption
requires moving just the suite's producers into this entrypoint; the normal CI
entrypoint keeps every check and runs its heavy matrix once per promotion.

release.yaml needs no change: credentials and publication still follow receipt
validation and cleanup. Its independently rebuilt outputs retain the existing
provenance requirements and are not certified as the preflight's tested bytes.

The host harness verifies complete suite/guest bytes, invokes every expected
case, checks exact inventory, ends QEMU through private QMP and joins child
processes. Cancellation kills and joins process groups. The compact termination
report binds immutable intent, canonical completed inventory and artifact-set
hashes; it fits kubelet's 4 KiB limit. The report is data from an attested pinned
harness, not a substitute for kubelet status.

The server checks Workflow identity, the emitted Pod and container specification,
owner UID, Pod UID, actual container ID, pinned image ID, exit zero, zero
restarts, and nonzero start/finish within the admitted absolute deadline. It
journals normalized actual termination plus the compact report in
`vm_executions` (v20). Terminal success or bounded refusal receipts cannot be rewritten or exchanged.
Refusals preserve available actual kubelet records (including nonzero exits and
restarts), while missing/failed evidence can never open the delivery gate. Shared VM capacity
remains reserved until exact owned Workflow/Pod/PVC cleanup is observed.
Uncertain creates or cleanup preserve the obligation; recovery cleans and
interrupts rather than manufacturing success. An unbound submission can be
rejected on recovery only after at least 60 continuous seconds of successful
exact-name Workflow, source-claim and source-consuming/seed Pod absence checks,
within a 90-second observation budget. Auxiliary presence resets that interval;
API errors retain the obligation. This path does not require the old checkout.
Individual recovery failures are logged and left pending without stopping the
scheduler; pending rows without a configured executor still block startup.
Explicit API Invalid/Forbidden/BadRequest admission rejection is definitive only
with follow-up Workflow absence and observed auxiliary cleanup.

Database triggers enforce immutable intent/identity, one-way Workflow UID and
receipt/refusal assignment, irreversible cleanup, and journal retention even for
raw SQL writes. Store methods retain the audited lifecycle checks.

Only after the successful receipt and cleanup does the scheduler record the
main Job intent and create the main CI or credentialed release Workflow. Thus
suite failure prevents release secret delivery and release publishing commands.
The main workflow currently rebuilds artifacts: the receipt proves the exact
preflight source/build outputs, not byte identity of an independently rebuilt
release binary. Release pipelines remain responsible for their existing signed
build/provenance rules. Publication independently rereads current retained
policy and the immutable journal. Branch publication must equal the tested
candidate; release publication must equal the admitted annotated tag object.
An unrelated default-branch advance changes the admitted baseline SHA and
conservatively requires a new run even if the policy text is unchanged.

## Harness packaging and rollout

`cmd/oberth-kvm-harness` is the actual static harness command. Run
`packaging/kvm-harness/build.sh OUTPUT_DIRECTORY` with `QEMU_RUNTIME` set to a
reviewed Linux/amd64 QEMU runtime image pinned by manifest digest. It emits a
binary, checksum, Dockerfile and exact base-reference record. The runtime must
supply `/usr/bin/qemu-system-x86_64`, its libraries, and public TLS trust roots.
An authorized image publisher builds this context with the same `QEMU_RUNTIME`,
verifies/scans/signs the result and installs its resulting digest into recipes.
The Dockerfile and server-owned trusted main container use UID/GID 65534,
RunAsNonRoot, dropped capabilities, no escalation, seccomp and a read-only root.
Operators must explicitly configure `--trusted-tests-kvm-group` (chart
`trustedTests.kvmSupplementalGroup`) to the actual positive device-plugin KVM
GID. Zero is the inactive default; configured recipes with no valid group fail
closed. No group is guessed from a node or candidate YAML, and there is no root
fallback. The leaf uses fsGroup 65534 with OnRootMismatch for its writable
private emptyDir independently of the supplemental device group. Producer
artifacts must be readable by this identity (and their parent directories
traversable); permission failures refuse the suite. Server-owned executor
helpers retain their separately attested identities and cannot mount the
private harness/result directory.

## Published images

The Oberth release pipeline (`.oberth/release.yaml`, template
`release-publish-kvm-harness`) builds, pushes and cosign-signs two images per
release to `europe-west4-docker.pkg.dev/skipopsmain/oberth/`:

| Image | Repository suffix | Contents |
|-------|-------------------|----------|
| Runtime | `oberth-kvm-runtime` | Alpine 3.22 apko image: `qemu-system-x86_64` 10.0.0-r1, firmware (`bios-256k.bin`, `linuxboot_dma.bin`, `kvmvapic.bin`), `ca-certificates-bundle`. No shell, no package manager beyond busybox. |
| Harness | `oberth-kvm-harness` | Runtime + `/usr/local/bin/oberth-kvm-harness` (0555). UID/GID 65534, `RunAsNonRoot`. |

Both are single-platform `linux/amd64` manifests (not multi-platform indexes).
The runtime image is built reproducibly with apko from
`packaging/kvm-harness/runtime.apko.yaml` and its lockfile; the harness layer
is appended with go-containerregistry. Digests are recorded in the release's
`release.json` under `images.kvmRuntime` and `images.kvmHarness`, and
verified by `release-verify` (signature + readback).

To install a published harness digest into `trustedTests.recipes`, use the
platform-manifest digest printed by the release step (the `@sha256:...` form
from `kvm-harness-image.txt`), not a tag or index digest.

### Registry authentication

`release-publish-kvm-harness` is admitted with the `image-writer` WIF role.
`prepare_kvm_registry_auth()` delegates to the shared `prepare_registry_auth`
path, which installs the public Docker credential-helper mapping. The helper
uses the `oberth-release-images` audience under project `138624361369`, provider
`tuxbox`, and the release-only projected token. It exchanges through STS and
`cloudtaser-release-publisher@skipopsmain.iam.gserviceaccount.com` to the fixed
`oberth-release-publisher@skipopsmain.iam.gserviceaccount.com` target, using
600-second IAM tokens and the existing scoped delegation. Only the final token
enters the credential protocol pipe; no registry login or access-token file is used.

The publisher still receives `cosign-secret` through Secretstore into private
tmpfs to sign both images. Registry authentication fetches no static GAR key;
refused WIF exchanges fail closed. Retired GAR paths and grants must not be
recreated. See [Publication prerequisites](../.oberth/bootstrap.md#publication-prerequisites)
for all three roles, the exact audience format and credential custody.

Deployment qualification must establish kubelet/runtime support for
`SupplementalGroupsPolicy: Strict` and refusal on unsupported nodes. Older
kubelets can silently merge image groups despite the requested Strict policy.
Observation attests the requested leaf Pod security context, main UID, fsGroup
and device group; it does not inspect effective runtime groups or prove that
unsupported nodes refuse the Pod.
Changing the configured group can block pending attestation until reconciled.
Source tests cover configured template fields and Pod-spec observation; they
do not qualify effective nonroot permissions, a device plugin, actual KVM
device GID, kubelet Strict support or live volume ownership.
The source contains no placeholder runtime digest, mutable fallback, secret or
automatic publishing action.

Deployment/release of the harness image, independently reviewed suite+guest
bundles, KVM device-plugin capacity and a live accepted Pod/receipt are rollout
prerequisites. Source tests do not claim those artifacts have been published or
that live KVM execution has been observed. Promotion 939's earlier failure was
a Git merge conflict in deployment/serve flag insertions before any CI run;
the other session rebased that source. That branch's prior green CI was not
trusted-suite acceptance evidence and it remains outside this implementation.

## Capacity waiting and evidence scope

A refused VM reservation with no own journal row parks the parent while holding
no weighted admission, concurrency-group or dispatch permit. The scheduler
rechecks after a bounded five-second wait, reacquires resources in original
claim order, then rereads cancellation, mutation authorization and retained
policy before constructing a fresh intent/deadline. At most MaxConcurrent
parked claims are exempt from dispatch accounting. Once this bounded allowance
is exhausted, later queued work can be backpressured even though parked runs
hold no weighted resources or group permits. Existing or uncertain
journaled executions never enter this retry path. Owner restart interrupts
parked undelivered parents; it cannot resume directly into release delivery.

Acceptance explicitly covers source identity, the credential-free preflight
artifact snapshot and its suite outcome. **It does not certify byte identity of
artifacts rebuilt by the later credentialed release workflow.** Those rebuilt
bytes cannot overwrite the immutable receipt; no equality comparison is made.
Consumers requiring tested-byte release identity need an independently reviewed
artifact-promotion/provenance contract before treating this gate as that proof.
The regression tests exercise a passing release with different rebuilt bytes
and verify that the receipt still identifies only the preflight snapshot.

Contract decoding rejects artifact paths that cannot be represented by USTAR's
100-byte name or 155-byte prefix plus 100-byte suffix fields. HTTPS fixture tests
verify trusted TLS 1.3 succeeds, TLS 1.2/untrusted authorities fail, and redirects
are refused without contacting their destination.

Protected-only requests require the exact `nonroot-static-v1` controller profile
before producers run, even when the repository declares no ordinary nonroot
leaves. The private proof binds the full trusted intent and is rechecked before
seeding, creation and adoption. Build rejects a missing, stale or substituted
proof; ordinary producer templates are not thereby converted to nonroot.
The executor image and runtime-hook attestation remains unchanged. Use the
explicit supported upgrade profile transition described in
[nonroot test leaves](nonroot-test-leaves.md) to prepare an existing managed
controller; setting the server profile name alone is not qualification.

## Owner restarts

Reserved trusted preflights use [durable restart recovery](trusted-preflight-restart.md).
After exact cleanup, the undelivered parent is interrupted and may receive one
fresh attempt under its original admission order. No exec stream or old receipt
is adopted, and uncertain cleanup retains capacity. This is separate from the
capacity-wait loop above and never retries a known suite failure automatically.
