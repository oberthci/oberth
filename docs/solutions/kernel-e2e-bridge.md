# Kernel-to-E2E Bridge Tier

Design for closing the structural test gap between QEMU kernel-isolation tests
and full-stack Kubernetes tests (Oberth issue #375).

## Problem

A workload eBPF test architecture has three tiers:

| Tier | What runs | Kernel coverage | Stack depth |
|------|-----------|-----------------|-------------|
| Unit tests | Go tests + C tests in CI container | Host kernel only | Agent code paths, no BPF load |
| QEMU smoke (`smoke-test.sh`) | Agent binary + BPF load + attack binaries in virtme-ng VM | 9 variants x 4 kernel versions | Agent in isolation -- no k3s, no pods |
| k3s canary (`k3s-openbao-test.sh`) | k3s + eBPF DaemonSet + OpenBao + wrappersim in virtme-ng VM | cloud-standard only (5.15/6.1/6.12/6.19) | Full Kubernetes lifecycle |

The gap: **no test tier exercises the full Kubernetes stack on non-stock kernel
variants.** The smoke test proves BPF loads and enforcement works per-variant,
but in process isolation -- no pod scheduling, no DaemonSet lifecycle, no
containerd-shim overlay mounts, no kubelet readiness probes driving
ReactiveKill evaluation. The k3s canary proves the full stack works, but only
on cloud-standard kernels.

Bugs that live in this gap (confirmed or narrowly caught):

- **ReactiveKill system-process SIGKILL (#458/#462):** Required k3s + eBPF
  agent + monitored wrapper pod + kubelet readiness probes to reproduce.
  Kernel-dependent: the LSM hook attach failure path differs by variant
  (EBUSY on cloud-standard in virtme-ng, genuine LSM absence on pre-bpf-lsm).
  Caught by the k3s canary only after it was added -- the smoke test could
  never have exposed it.

- **containerd-shim overlay mount EPERM (#408):** BPF LSM file_open hook
  blocked overlayfs mounts from containerd-shim. Required a real container
  runtime to surface. Only the k3s canary's overlay-smoke pod caught it.

- **Verifier complexity on backport kernels (6.12.90+, 6.19+):** The smoke
  test catches BPF verifier rejections, but deployment-time behavior (e.g.,
  CrashLoopBackOff from `isVerifierComplexityError` pattern mismatch) requires
  a DaemonSet restart loop to observe.

- **Policy-map handover (#269):** Agent SIGKILL + restart + pinned-map reuse
  requires k3s to reschedule the DaemonSet pod. Kernel-variant-specific
  because pinned BPF link behavior varies by kernel version.

## Design: extend k3s canary to multi-variant kernel matrix

### Why not alternatives

**(a) KubeVirt with cloud kernels:** The KubeVirt path (`test/kernel/kubevirt/`)
already exists as a prototype. It boots a full Ubuntu 24.04 VM via KubeVirt
containerDisk. However: KubeVirt runs on the host kernel's KVM -- the guest
kernel is whatever the cloud image ships. To test variant kernels, we would
need to build per-variant cloud images (8+ images, each 2-4 GB), maintain a
KubeVirt cluster, and manage image versioning. High infrastructure cost for
the same test we can already run in virtme-ng.

**(b) Real cloud clusters:** Deploying the full application stack on GKE/EKS/AKS
with their stock kernels would test real production kernels but at prohibitive
cost (cluster-per-variant, multi-cloud credentials in CI, 15-30 min per
cluster spin-up). Reserved for release qualification, not per-commit CI.

**(c) QEMU full-stack (selected):** Extend the existing k3s-openbao-test.sh to
run on additional kernel variants. The infrastructure already exists: virtme-ng
boots a configurable kernel, k3s runs inside, the eBPF agent deploys as a
DaemonSet, wrappersim exercises the wrapper lifecycle. The marginal cost of
adding a kernel variant to this matrix is one additional QEMU invocation per CI
run -- identical infrastructure, identical test script, different kernel binary.

### Architecture

```
                          smoke-test.sh                k3s-openbao-test.sh
                        (agent isolation)           (full Kubernetes stack)
                              |                              |
Kernel variant    BPF load + enforcement    k3s + DaemonSet + wrappersim + OpenBao
--------------------------------------------------------------------------
cloud-standard         [existing]                    [existing: 4 kernels]
cloud-secretmem        [existing]                    [NEW: bridge tier]
gke-cos                [existing]                    [NEW: bridge tier]
gke-ubuntu             [existing]                    [NEW: bridge tier]
rhel9                  [existing]                    [NEW: bridge tier]
debian-bookworm        [existing]                    [NEW: bridge tier]
cloud-lockdown         [existing]                    [deferred: k3s may not start]
minimal-bpf            [existing]                    [deferred: limited BPF]
pre-bpf-lsm           [existing]                    [deferred: k3s may not start]
```

### What runs inside each bridge-tier VM

Identical to the existing k3s-openbao-test.sh flow. The test script is
kernel-variant-agnostic by design -- the agent detects its own enforcement tier
at startup. What changes per variant is:

1. **The kernel binary** booted by virtme-ng (from the kernel image archive,
   issue #374)
2. **Expected enforcement posture** -- the test already handles this via the
   `/status` endpoint's `enforcement_mode` field

No code changes to k3s-openbao-test.sh itself are needed for the initial
bridge tier. The script already:
- Starts k3s with `--flannel-backend=none` and hostNetwork pods
- Deploys the eBPF agent as a DaemonSet with `REQUIRE_LSM_OR_FAIL=false`
- Starts OpenBao in dev mode on loopback
- Deploys wrappersim with SO_PEERCRED auth over unix socket
- Asserts zero SIGKILLs to system processes (bpftrace + agent self-report)
- Tests policy-map handover across agent SIGKILL (#269)

### Variant-specific assertions (new)

While the test script itself is variant-agnostic, the bridge tier adds
post-test assertions that verify the enforcement mode matches expectations:

```bash
# After Step 13 (zero SIGKILLs assertion), query /status and verify
# enforcement_mode matches the variant's expected posture.
STATUS=$(curl -sfk "https://localhost:19099/status" 2>/dev/null || echo '{}')
ENF_MODE=$(echo "$STATUS" | jq -r '.enforcement_mode // "unknown"')

case "$KERNEL_VARIANT" in
    cloud-standard|cloud-secretmem|debian-bookworm|rhel9|gke-cos)
        # Tier 2: LSM available but no kprobe_override -> reactive_kill or lsm
        assert_enforcement_mode "$ENF_MODE" "reactive_kill|lsm|detect-only" ;;
    gke-ubuntu)
        # Tier 1: full enforcement expected
        assert_enforcement_mode "$ENF_MODE" "full|kprobe_block" ;;
esac
```

This assertion catches the class of bug where enforcement LOADS correctly (smoke
test green) but BEHAVES incorrectly under Kubernetes lifecycle pressure (pod
scheduling, readiness probes, DaemonSet restarts).

### CI workflow integration

The bridge tier adds a new Argo workflow template `kernel-test-k3s-bridge.yaml`
(or extends the existing `kernel-test-k3s.yaml`) with a matrix strategy:

```yaml
# Conceptual -- keep actual YAML in the workload repository's .oberth/ directory
strategy:
  matrix:
    variant:
      - cloud-standard    # [existing, already covered]
      - gke-ubuntu        # Tier-1 gate: synchronous kprobe deny
      - gke-cos           # GKE COS: LSM-only, no kprobe_override
      - rhel9             # OpenShift: RHEL-backported BPF LSM
      - cloud-secretmem   # CONFIG_SECRETMEM=y variant
      - debian-bookworm   # Debian stable, mirrors cloud-standard behavior
    kernel_version:
      - "6.1"             # LTS: EKS default, GKE stable
      - "6.12"            # LTS: Debian bookworm backport, recent cloud
```

### Priority ordering

Not all variant x kernel combinations are equally valuable. Priority:

1. **gke-ubuntu x 6.1/6.12** -- Tier-1 enforcement gate. This is the only
   variant where kprobe synchronous deny is expected to work. A regression
   here means the product's strongest enforcement tier is broken under
   Kubernetes lifecycle. Release-blocking.

2. **rhel9 x 5.14-backport** -- OpenShift customers. RHEL's 5.14 kernel
   backports BPF LSM but not kprobe_override. The k3s canary must prove the
   agent correctly falls back to reactive-kill without SIGKILLing system
   processes on this kernel.

3. **gke-cos x 6.1** -- GKE Container-Optimized OS. Second most common
   production target after GKE Ubuntu. LSM-only tier.

4. **debian-bookworm x 6.1/6.12** -- Debian stable. Mirrors cloud-standard
   but with CONFIG_SECRETMEM=y. Catches memfd_secret path divergences.

5. **cloud-secretmem x 6.12** -- Explicit CONFIG_SECRETMEM test. Lower
   priority because debian-bookworm covers the same kernel config space.

### Deferred variants

**cloud-lockdown, minimal-bpf, pre-bpf-lsm** are deferred from the bridge
tier. These variants have restricted BPF capabilities that may prevent k3s
itself from starting (k3s uses BPF for kube-proxy replacement, CNI, and
cgroup management). The smoke test's agent-isolation coverage is sufficient
for these extreme-degradation variants -- the bridge tier targets the variants
where customers actually deploy the application.

### Implementation phases

**Phase 1: gke-ubuntu bridge (release-blocking gate)**

- Prerequisite: kernel image archive (issue #374) provides pre-built gke-ubuntu
  kernel binaries for virtme-ng
- Extend kernel-test-k3s.yaml matrix to include `gke-ubuntu` variant
- k3s-openbao-test.sh already handles gke-ubuntu (REQUIRE_LSM_OR_FAIL=false,
  agent detects kprobe_override availability)
- Add post-test enforcement-mode assertion (expect `full` or `kprobe_block`)
- CI time budget: +15-20 min per kernel version (QEMU boot + k3s start +
  tests + teardown under TCG emulation; ~8-10 min under KVM)
- Gate: release-blocking for any new eBPF agent tag

**Phase 2: cloud provider variants (rhel9, gke-cos, debian-bookworm)**

- Add 3 more variants to the matrix
- Each variant uses its kernel image from issue #374
- Total additional CI time: 3 variants x 2 kernel versions x ~15 min =
  ~90 min under TCG. Parallelizable across separate Argo workflow steps
  (each step boots its own QEMU VM)
- Gate: CI-informational initially, promoted to release-blocking after one
  month of stability

**Phase 3: operator + real wrapper (stretch goal)**

- Replace wrappersim with the actual workload wrapper binary
- Deploy the workload operator (without the mutating webhook -- direct
  DaemonSet + wrapper deployment, no cert-manager dependency)
- This would close the remaining gap: operator -> webhook -> wrapper -> agent
  registration -> enforcement, all on a non-stock kernel
- Requires cross-repo binary staging: operator and wrapper binaries built and
  injected into the VM alongside the eBPF agent
- Deferred until the wrappersim-based bridge tier is stable

### VM composition

Each bridge-tier VM receives (via virtme-ng `--root` and 9p mounts):

| Component | Source | Size |
|-----------|--------|------|
| Kernel binary | kernel image archive (#374) | 10-30 MB |
| k3s binary | Pre-fetched in CI (pinned version) | ~70 MB |
| OpenBao binary | Pre-fetched in CI (pinned version) | ~120 MB |
| eBPF agent binary | Built from source in the same CI run | ~30 MB |
| BPF object (`secret_monitor.o`) | Compiled from source (clang-19) | ~2 MB |
| wrappersim binary | Built from source in the same CI run | ~15 MB |
| Attack test binaries | Built from source (leaktest, attacktest, etc.) | ~10 MB |
| busybox + pause images | Pre-cached tarballs for k3s airgap | ~5 MB |

Total per-VM payload: ~280 MB. The VM boots with 2-4 GB RAM (k3s + kubelet +
containerd + eBPF agent + OpenBao + wrappersim pods). No disk image needed --
virtme-ng uses the host filesystem via 9p with tmpfs overlays for writable
state (`/var/lib/rancher/k3s`, `/var/lib/kubelet`).

### Result reporting

Each bridge-tier VM run produces:

1. **Gate result:** `K3S_OPENBAO_GATE = 1` (pass) or `K3S_OPENBAO_GATE = 0`
   (fail), same as the existing k3s canary
2. **Enforcement mode attestation:** The agent's `/status` enforcement_mode
   field, logged and compared against the variant's expected posture
3. **Structured log output:** All test steps (k3s start, eBPF Ready, OpenBao
   round-trip, wrappersim registration, zero-SIGKILLs, policy handover)
   with PASS/FAIL per step, captured by the Argo step log
4. **bpftrace evidence:** When available, syscall-level SIGKILL trace proving
   zero kills to system processes

The Argo workflow aggregates results across the matrix. Any single variant
failure blocks the release (Phase 1) or is surfaced as a CI issue (Phase 2).

### CI time budget

Current k3s canary (cloud-standard, 4 kernel versions):

| Phase | Time (TCG) | Time (KVM) |
|-------|------------|------------|
| QEMU boot + k3s start | 3-5 min | 1-2 min |
| eBPF DaemonSet Ready | 1-2 min | 0.5-1 min |
| wrappersim lifecycle | 2-3 min | 1-2 min |
| ReactiveKill + handover | 3-5 min | 2-3 min |
| Total per VM | 10-18 min | 5-8 min |

Bridge tier (6 variants x 2 kernel versions = 12 runs, parallelized):

| Parallelism | Total wall time (TCG) | Total wall time (KVM) |
|-------------|----------------------|----------------------|
| Serial | 120-216 min | 60-96 min |
| 4-wide | 30-54 min | 15-24 min |
| 6-wide | 20-36 min | 10-16 min |

The tuxbox host has 12 cores. Each QEMU VM uses 2 vCPUs, so 6-wide
parallelism is feasible but saturates the host. Recommendation: 4-wide for
branch CI (leaves headroom for other runs), 6-wide for release-gate runs.

### Failure modes and mitigations

| Failure mode | Impact | Mitigation |
|--------------|--------|------------|
| Kernel binary missing from archive | Bridge test skipped | Issue #374 tracks kernel archive; bridge tests fail-open until archive is populated |
| k3s won't start on variant kernel | False positive | Pre-validate each kernel variant can boot k3s before adding to release-blocking matrix |
| QEMU timeout (TCG slowness) | Flaky test | Separate watchdog timers: per-step (bounded), per-VM (outer `timeout N vng`), existing pattern from smoke-test.sh |
| Host CPU saturation | All VMs slow | Cap parallelism at 4-wide for branch CI; the Oberth `oberth.ci/size: L` annotation reserves appropriate resources |
| eBPF agent hits verifier complexity | True positive | This IS the bug we want to catch -- the bridge tier surfaces it with full-stack context instead of just a BPF_LOAD_OK gate |

### Dependencies

- **Issue #374 (kernel images):** The bridge tier cannot run without pre-built
  kernel binaries for virtme-ng. The smoke test already consumes these for
  agent-isolation tests; the bridge tier uses the same kernel archive.

- **k3s binary caching:** The k3s binary is large (~70 MB). The existing
  kernel-test-k3s workflow already caches it; the bridge tier reuses that
  cache.

- **QEMU/virtme-ng availability:** The host must have QEMU and virtme-ng
  installed. Already a requirement for the existing kernel test matrix.

### Success criteria

The bridge tier is complete when:

1. Every release tag runs the bridge-tier matrix as part of the eBPF release
   pipeline
2. At least `gke-ubuntu` is release-blocking (Phase 1)
3. The bridge tier has caught at least one real bug that the smoke test alone
   would not have surfaced (or retrospectively would have caught #458/#408)
4. The bridge tier runs without flakes for 10 consecutive releases
5. CI time increase is within the budgeted 30 min wall time (4-wide
   parallelism under TCG)

### Non-goals

- Deploying the real operator + webhook + cert-manager (Phase 3, stretch goal)
- Testing customer Helm chart values or upgrade paths
- Running on real cloud provider kernels (reserved for release qualification)
- Testing arm64 in the bridge tier (virtme-ng arm64 under TCG is prohibitively
  slow; arm64 kernel testing remains in the smoke tier)
- Replacing the existing smoke test or KubeVirt prototype

## References

- Oberth issue #374: Kernel image archive for CI
- Oberth issue #375: This issue (kernel-to-e2e test tier gap)
- Workload-owned agent-isolation kernel tests, existing k3s canaries and
  KubeVirt prototypes establish the application-specific acceptance evidence.
- Historical failure classes include reactive-kill regressions, containerd-shim
  overlay permission errors, and policy-map handover across agent termination.
