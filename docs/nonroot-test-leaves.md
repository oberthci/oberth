# Ordinary nonroot test leaves

`oberth.ci/nonroot-templates: postgres-tests,browser-tests` is a static workflow
annotation selecting named, top-level ordinary container/script templates.
This source draft is disabled unless the server establishes the exact
`nonroot-static-v1` controller profile. The normal installer option
`--argo-controller-profile=nonroot-static-v1` prepares the pinned upstream
chart1.0.24 with an immutable, content-addressed public configuration and fixed
controller/executor image digests. The chart/server profile name is a request to
verify support, never sufficient proof. Custom/reused settings outside the
reviewed profile, a different chart, or `--skip-argo-workflows` are refused.
No installation or supporting release has been performed by this source change.
The server sets their fixed UID/GID 65534 and security policy. It is not a request
for arbitrary user IDs, groups, capabilities, Pod patches or security contexts.
The first implementation rejects credentialed workflows, template defaults and
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
static mode. Workflow archiveLogs is rejected. Controller-forced archive logging
is rejected by exact public configuration verification: Argo gives that setting
priority over workflow/template false and can inject artifact configuration.


Before audit, source seeding, Workflow creation and existing-run adoption, the
server checks one Ready controller, fixed command/environment/image identity,
and the one named immutable public ConfigMap through narrowly scoped GET.
The required `config` key is mounted read-only only in the trusted controller at
`/var/run/oberth-controller-profile`; no subpath, optional key, alternate source,
extra/overlapping mount or sidecar is supported. The only other controller mount
is the enumerated standard Kubernetes service-account projection (token with
3607-second requested lifetime, root CA and namespace). This is a deliberately
restricted supported profile, not generic compatibility with arbitrary token
infrastructure; it introduces no Secret or new token permission.

The required mount gates process startup. Authenticated Argo4.0.8 synchronously
GETs and parses that same configuration in its controller constructor and exits
on failure before starting workers. This causal dependency permits equal-second
API/process timestamps; a later-created ConfigMap still rejects. This reasoning
assumes trusted installation, immutable content-addressed names never deleted
and reused, and trusted cluster clocks/infrastructure. Read-only verification
cannot atomically freeze Kubernetes between calls or defend against a malicious
administrator. An observed controller/config UID or resourceVersion change
invalidates the request proof. The opaque proof binds the full request source,
fragment bytes and identity before audit; normal SourceVolume assignment does
not change that request identity. Recovery checks the actual persisted policy,
volume/template spec and profile binding, not just matching commit annotations.

There is currently no supporting released server. Oberth v0.13.42 accepts and
ignores this annotation and still forces UID0. The minimum supported release
must be the first authorized release containing the reviewed #303 fix (the next
currently unused version, v0.13.43, is only a proposal). Do not activate consumer
workflows before that release is deployed and the substrate is verified. Every
initial fixture lane must fail before its tests unless both `id -u` and `id -g`
equal 65534. Missing-runtime or skip-capable fixture code is not a substitute for
this check, and a version label alone is not execution proof.

One fixed initializer uses the administrator-owned, digest-pinned source-seed
image. It runs as UID 0 with dropALL, RuntimeDefault, read-only root and no
privilege escalation, and prepares only fresh per-Pod EmptyDirs. It has no source,
PVC, host path or token mount. It makes the short test `/tmp`, Argo control files,
executor `/tmp` subpath 0 and optional script staging writable, then exits. It does
not chown shared data or set fsGroup. Main and normal Argo init/wait then run as
65534 with the same restrictions. Argo's existing executor token mounts stay
confined to its init/wait; main remains tokenless. The fake-controller contract
uses upstream's existing Secret-volume token fixture; this change neither
creates sensitive Kubernetes Secret material nor changes token infrastructure.
Preparation can replay after partial initialization. An existing executor
subdirectory must be a real directory owned by the initializer; symlinks,
files and foreign ownership fail before chmod. The enclosing executor EmptyDir
stays root-owned and unavailable to main, so main cannot replace subpath 0.

Selected leaves read source and shared tool inputs without write access, including
Argo's mirrored input mounts. They have no persistent host cache mount.
TMPDIR/GOTMPDIR are `/tmp`, GOCACHE is `/tmp/gobuild`, GOMODCACHE is `/tmp/gomod`,
and HOME/XDG caches also use this per-Pod scratch. Its 8Gi bound and Argo's separate
control/staging bounds prevent unbounded writable storage. The Argo emissary
`var-run-argo` EmptyDir retains stdout/stderr and control files until Pod cleanup;
it defaults to 1Gi. Administrators can set `argo.executorScratchSizeLimit` in the
chart (`serve --argo-executor-scratch-size-limit`, a positive Kubernetes quantity).
This limit also applies to the protected trusted KVM leaf and is checked by its
Pod attestation. Trusted KVM Pods created before a sizeLimit change fail recovery
attestation and must be re-run under the new limit. It is separate from `/tmp`
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
strategic merge can discard that map, which the exact-profile regression proves. Argo version, module sums and
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
source tests proves KubeVirt, guest enforcement or deployed browser coverage.

## Explicit profile transition on an existing installation

`oberth upgrade --argo-controller-profile=nonroot-static-v1` is an opt-in
transition of both existing managed releases. It also runs when the Oberth
version is unchanged; omitting the flag preserves the normal same-version
no-op. The pipeline namespace is read from the installed Oberth values. The
command refuses conflicting namespace/profile overrides, absent or unsupported
Argo releases, pending Helm operations and custom values outside the fixed
profile. It does not install namespaces, reconcile OpenBao or change uplinks.

Before mutation, review `--dry-run` and the complete rendered diff against the
current releases, and drain native execution. The command preserves reused
values, verifies the exact upstream Argo chart1.0.24 archive digest, and applies
only the built-in public profile overlay to the existing Argo release. It waits
for the same strict live controller/configuration proof used by submission
admission before upgrading Oberth and setting `argo.controllerProfile`.
The existing KVM recipe, harness pin and unrelated identities stay in reused
values. Final success requires the controller proof and Oberth profile value
and serving flag to agree after rollout.

This is a two-release forward transition, not an atomic transaction. Failure
after the Argo update leaves the stronger profile installed and reports an
incomplete transition. Do not roll back to an unverified controller. A fresh
reviewed invocation accepts the same exact profile and can repair an existing
failed Helm revision. A changed revision or values snapshot before apply is a
refusal; the read checks do not lock out another administrator. No protected
suite result is implied by installation: its exact run still needs a valid
receipt and confirmed cleanup.

The transition checks the Argo revision, status, chart and unchanged raw reused
values both before chart resolution and immediately before the upgrade. These
observations are not an atomic cross-release lock. Final server verification
waits for exactly one ready Pod (including waiting out terminating old Pods),
binds its ReplicaSet and Deployment UID/template plus image and arguments,
executes the version check by that Pod name, and rechecks its UID. Live rollout
evidence additionally checks the actual platform image digest. Partial Argo or
Oberth failure is never reported as success; recovery is a fresh supported
forward transition, not automatic rollback.

Upgrade `--set` flags accept one canonical dotted key and scalar value each.
Repeat flags instead of comma-separated assignments; a literal value comma
can be escaped as `\,`. List syntax and escaped keys are rejected before
mutation. For structured values use an explicitly reviewed `--values` file;
protected image/profile paths remain refused there as well.

Retained Failed/Succeeded controller Pods do not represent live controllers
when their complete current termination status proves otherwise. Selection
requires the fixed single-controller shape, matching runtime IDs, false
readiness, ordered start/finish times and coherent prior restart history.
Names and UIDs must be unique, and timestamps cannot exceed the selection's
single captured observation time.
Unknown or synthetic current termination, missing statuses, terminating Pods,
and multiple active candidates remain refusals. The selected Running Pod still
passes the full existing profile verification. This relies on trusted API and
kubelet status and does not authorize deleting historical Pods to unblock an
upgrade. If an older CLI has stopped after applying Argo because it counted
terminal history, preserve both releases and use a freshly reviewed supported
forward transition with the corrected signed CLI; do not roll back or repeat
the unchanged command.
