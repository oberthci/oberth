# Platform R2: local pressure admission/re-creation qualification

2026-10-31. PASS using the cached `kindest/node:v1.36.4` image and
`bash hack/qualify-platform-pressure.sh /absolute/evidence-directory`.
This is a disposable local fixture, not tuxbox or an Oberth service rollout.

The script creates only its own named kind cluster and an explicit kubeconfig;
it refuses an existing cluster of that name. The cached pause image uses
`imagePullPolicy: Never`. It mounts no host data or target credentials and deletes
the cluster on exit. Pod tests use no ServiceAccount token.

To exercise real kubelet admission without exhausting host resources, the
fixture uses a synthetic **soft** `memory.available` or `nodefs.available`
threshold of 100Ti and a one-hour eviction grace. This reports a real kubelet
pressure condition/taint immediately without subtracting an impossible hard
reservation from allocatable or evicting unrelated fixture services. Effective
configz, node conditions and pod status are retained. These are test-only values,
not proposed production thresholds.

Results:

| Case | Observed result |
| --- | --- |
| Critical class outside kube-system | Server dry-run in `platform-qualification` resolved system-cluster-critical to priority 2,000,000,000. |
| MemoryPressure | Untolerated BestEffort control was Unschedulable; explicit tolerant critical Pod became Ready while kubelet's condition and taint were present. |
| DiskPressure scheduler filter | Untolerated control was Unschedulable. |
| DiskPressure kubelet admission | Tolerant custom-priority (1,000,000) control was bound to the node, then Failed/Evicted by kubelet. Tolerations alone were insufficient. |
| Re-created platform stand-ins | Four pre-existing critical Pods were deleted and re-created under DiskPressure; each had a different UID, priority 2,000,000,000, Guaranteed QoS and Ready=True. |
| Recovery | Exact original kubelet file/extra-argument state restored, DiskPressure cleared, then the disposable cluster was deleted. |

Fixture development failures were not qualification passes: manually applied
pressure taints disappeared while the condition was false; literal 100% eviction
thresholds are ignored; a hard threshold above capacity can prevent kubelet
startup by producing invalid allocatable. The final run instead verifies the
effective soft thresholds and actual conditions before creating the controls.

Limits: these are **pause-container stand-ins**, not running Oberth, Argo,
OpenBao or cloudflared. They prove scheduler/kubelet admission and changed-UID
re-creation, not application health, unseal/token/DB/ingress recovery, actual
platform resource headroom, anonymous-memory exhaustion, page-cache behaviour,
target quotas or k3s 1.36.5. Those remain required operator qualifications in
the [R2 plan](platform-protection-plan.md). Source review also checked upstream
1.36.5's critical predicate and eviction admission path; no provider-specific
policy was assumed from the local namespace result.

Subsequent source verification confirms two additional rehearsal requirements:
Kubernetes 1.36.5's disk-backed emptyDir/pod/container storage-limit paths call
`evictPod`, whose critical guard refuses eviction; and KubeVirt v1.9.1's operator
manifest sets `kubevirt-cluster-critical` to 1,000,000,000, below the proposed
platform 2,000,000,000. The [plan](platform-protection-plan.md) now includes bounded
storage-limit and KubeVirt preemption cases. **Those extra cases were not run by
this original fixture**; its PASS must not be read as storage containment or
KubeVirt control recovery proof.

Final evidence retained outside Git (no kubeconfig, credentials or raw environment
values are included in the source bundle):

- `/tmp/fix/platform-r2-pressure-v5.log`
- `/tmp/fix/platform-r2-pressure-evidence-summary.json` (image ID, control verdict,
  per-stand-in old/new UIDs, priority, QoS and readiness; sanitized)
- `/tmp/fix/platform-r2-pressure-evidence-v5/` (local fixture working evidence)

The read-only target metrics command and the coordinator-supplied format PASS
are in [the cAdvisor probe note](kubelet-cadvisor-probe.md). The daemon's actual
direct-kubelet TLS/token route remains unqualified before sampler enablement.
