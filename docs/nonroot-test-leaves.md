# Ordinary nonroot test leaves

`oberth.ci/nonroot-templates: postgres-tests,browser-tests` is a static workflow
annotation selecting named, top-level ordinary container/script templates.
The server sets their fixed UID/GID 65534 and security policy. It is not a request
for arbitrary user IDs, groups, capabilities, Pod patches or security contexts.
The first implementation rejects workflows with secret-consuming steps, template defaults and
selected leaves with extra init/sidecar/plugin/artifact or nested Pod shapes.
Any declared SecretStore path rejects the entire workflow for this mode, even
if the selected test leaf does not itself read credentials. Existing credentialed
release.yaml cannot opt in one test leaf. Release equivalence needs a separately
supported uncredentialed validation chain and remains open; do not weaken this
initial admission restriction to match a release layout.

Selected templates must contain no parameter inputs or Argo substitutions.
Their JSON must fit 64 KiB both at admission and after every server injection,
including the server's Pod patch. Argo 4.0.8 offloads template/argument values
above 128 KiB into a ConfigMap mounted in every init container. The 64 KiB
margin accommodates fixed controller fields and the script argument while
keeping preparation confined to EmptyDirs. Repository Pod metadata is already
rejected by ordinary admission; workflow metadata is not substituted into this
static mode. Workflow-authored archive logging remains subject to admission.

One fixed initializer uses the administrator-owned, digest-pinned source-seed
image. It runs as UID 0 with dropALL, RuntimeDefault, read-only root and no
privilege escalation, and prepares only fresh per-Pod EmptyDirs. It has no source,
PVC, host path or token mount. It makes the short test scratch directory, Argo control files,
executor scratch subpath 0 and optional script staging writable, then exits. It does
not chown shared data or set fsGroup. Main and normal Argo init/wait then run as
65534 with the same restrictions. Argo's existing executor token mounts stay
confined to its init/wait; main remains tokenless. Pinned-controller fixtures retain the captured upstream token shape; live token
delivery uses Oberth's separate executor identity mechanism.
Preparation can replay after partial initialization. An existing executor
subdirectory must be a real directory owned by the initializer; symlinks,
files and foreign ownership fail before chmod. The enclosing executor EmptyDir
stays root-owned and unavailable to main, so main cannot replace subpath 0.

Selected leaves read source and shared tool inputs without write access, including
Argo's mirrored input mounts. They have no persistent host cache mount.
TMPDIR/GOTMPDIR and the GOCACHE/GOMODCACHE directories use private scratch,
and HOME/XDG caches also use this per-Pod scratch. Its 8Gi bound and Argo's separate
control/staging bounds prevent unbounded writable storage. The Argo emissary
`var-run-argo` EmptyDir retains stdout/stderr and control files until Pod cleanup;
it defaults to 1Gi. Administrators can set `argo.executorScratchSizeLimit` in the
chart (`serve --argo-executor-scratch-size-limit`, a positive Kubernetes quantity).
This limit is separate from private scratch
and CPU/memory step requests: reducing
a test's resource requests does not reduce its retained log output. Server log
collection limits do not cap emissary's on-disk copy. Size executor scratch for
the step's output as well as its declared resource requests.

Kubelet evictions (including executor emptyDir limits and node memory or
ephemeral-storage pressure) are infrastructure failures with the kubelet reason.
They retain failed step rows, permit `run_retry` for eligible branch runs, and
do not open, update, or resolve repository CI-red issues. A container killed at
its own declared memory limit remains a test failure; `OOMKilled` or exit 137/143
alone does not establish node pressure. Scheduler-side infrastructure failures
remain visible as CI issues on default-branch and scheduled runs; an independent
failed test still makes CI red. Tools produced by
earlier ordinary steps are read-only inputs; only the existing separate
OBERTH_ARTIFACTS mount remains available for explicit publishable output. Cache
files are not collected as artifacts. Cold caches are the initial compatibility
tradeoff; this mode does not alter another step's cache permissions.

The ordinary Go suite verifies admission, final size boundaries, preparation
replay and the current Build/patch against pinned controller observations in
`internal/argojob/testdata/nonroot-argo4.0.8`. The fixtures include raw Pods just
before `processPodSpecPatch`, and the actual submitted Pods for container and
script shapes. The test replaces Build-owned main/template/volume fields with
current output, applies the current strategic patch, then reproduces only the
fixed later emissary and executor/staging mount mutations documented in that
directory. It compares the complete result with the observed submitted Pod and
uses hostile and absent executor-context controls. Every security-context field
is explicitly set or cleared, with a reflection guard for Kubernetes schema
additions. Nested `$patch: replace` alone is unsafe for an absent original map:
strategic merge can discard that map, which the pinned-controller regression proves. Argo version, module sums and
fixture hashes are guarded: upgrades require fresh real-controller proof.

The original local capture used the exact authenticated Argo 4.0.8 controller
with upstream fake clients and a test-only overlay, and inspected its actual
submitted Kubernetes object. No cluster was contacted or module cache modified.
Its separate compiled test dependency closure contains vulnerable x/crypto and
gRPC versions; that toolchain is not shipped or provisioned by this change.
The bounded fixture test runs in the ordinary repository test graph. It does
not rerun upstream controller construction, exercise admission webhooks or
prove kubelet execution. Those limits remain explicit release prerequisites.

Actual PostgreSQL startup/query/cleanup and the seven db-proxy cases remain
separate acceptance work. Chromium additionally needs proven user namespace,
clone/seccomp and kernel compatibility under the real RuntimeDefault container;
UID65534 alone is insufficient. No SYS_ADMIN, privilege escalation, unconfined
seccomp or `--no-sandbox` workaround is part of this capability. None of these
source tests proves external-workload enforcement or deployed browser coverage.


The operator owns Argo controller configuration. Oberth no longer attests its
image, process, Pod history or ConfigMap, and dispatch does not wait for a
controller-profile proof. Selected nonroot leaves retain the fixed preparation
and security patch described above. Containers injected by operator controller
`templateDefaults` or `workflowDefaults`, including extra sidecars and init
containers, are operator-owned and are not hardened by this patch. Repository
admission does not inspect those controller defaults.

The patch depends on the Argo 4.0.8 generated Pod names and paths. A different
Argo version that changes those shapes fails closed at Pod creation when the
resulting patch produces an invalid Pod; dispatch no longer verifies the version
in advance. This is a Pod-creation refusal, not a guarantee that every other
controller version is detected or that operator-added containers are hardened.
Qualify controller changes against the resulting Pod shapes before deploying them.
