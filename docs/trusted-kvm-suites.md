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
and no user configuration. Suite authors provide actual guest initialization
and host assertions; Oberth does not fabricate a passing generic guest.

## Execution, evidence and releases

Oberth runs `.oberth/build.yaml` in a fresh credential-free preflight Workflow,
then appends the server-owned pinned `trusted-tests` leaf. Candidate secret,
WIF, hook, retry, daemon and template-default capabilities are refused there.
The trusted leaf receives only a read-only artifacts mount and private scratch.
After producers complete, the harness copies admitted regular files into private
bounded snapshots, hashing each path, size and byte content in contract order.
Symlinks, escapes and special files are rejected. Candidate bytes never execute
in the host harness.

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
interrupts rather than manufacturing success.

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
The Dockerfile defaults to UID 65534; the server-owned KVM leaf explicitly
uses UID 0 for the device-plugin device permissions, with all capabilities
dropped, escalation disabled, seccomp enabled and a read-only root filesystem.
The source contains no placeholder runtime digest, mutable fallback, secret or
automatic publishing action.

Deployment/release of the harness image, independently reviewed suite+guest
bundles, KVM device-plugin capacity and a live accepted Pod/receipt are rollout
prerequisites. Source tests do not claim those artifacts have been published or
that live KVM execution has been observed. Promotion 939's earlier failure was
a Git merge conflict in deployment/serve flag insertions before any CI run;
the other session rebased that source. That branch's prior green CI was not
trusted-suite acceptance evidence and it remains outside this implementation.
