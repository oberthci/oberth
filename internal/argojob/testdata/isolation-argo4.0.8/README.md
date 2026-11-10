# Controller observations

These JSON files are raw inputs and observations from the authenticated Argo
Workflows v4.0.8 module. `provenance.json` records module and file checksums.
The controller's real `operate(ctx)` processed each current Oberth `Build`
output with upstream fake clients. The pipeline ServiceAccount deliberately
has automount enabled; a distinct executor ServiceAccount has a fixture token.
The only controller-source overlay calls a capture callback immediately before
`processPodSpecPatch`. The final Pod comes from the fake client's stored Pods.

The dedicated-tools case covers a separate tools PVC, an overlapping parent
alias and a verification init container. All eight final Pods also passed Kubernetes v1.35.7 `ValidatePodCreate`, after
standard API defaulting and v1-to-internal conversion. The validator source,
module graph, and output are retained as text. Fake-client acceptance alone is
insufficient: initial sidecar captures had duplicate mount paths, and initial
container-set captures had missing volume definitions. Both were rejected by
the real validator before the server repairs.

To regenerate, copy the two `*_test.go.txt` helpers to the scratch directory
named inside them (or update that path consistently). Use a Go overlay to add
the generator as `internal/argojob/zz_isolation_export_test.go` and run
`TestExportIsolationControllerInputs`. In the authenticated Argo module, use
a second overlay to add `capture_test.go.txt` as
`workflow/controller/zz_oberth_isolation_test.go` and overlay `workflowpod.go`
with this one insertion before `podSpecPatchs, err = woc.processPodSpecPatch`:

```go
if isolationCapture != nil { isolationCapture(pod.DeepCopy()) }
```

Run `TestOberthIsolationControllerCapture`, then run the retained Kubernetes
validator against every `*-submitted.json`. Both must pass. Copy the input,
before-patch and submitted JSON, recording fresh checksums and validator output.
Do not edit expected Pods manually to make tests pass. Use a separate scratch directory for build
scratch and cap Go concurrency; no live API or module-cache edits are needed.

The normal regression checks that current Build output still equals the
captured input, checks the submitted mounts/identity/environment, and applies
the current server patch to the observed writable mirrors as a negative-control
boundary test. A changed input requires fresh controller proof. This is not a
test of custom cluster admission webhooks, image execution, or artifact fetches.

The 2026-10-19 recapture for #962 used a combined overlay containing the retained
isolation, WIF and nonroot capture helpers. Immediately before the controller
patch call, it invokes both `isolationCapture` and `oberthWIFCapture` when
non-nil. Set `OBERTH_ISOLATION_PROOF_DIR`, `OBERTH_WIF_PROOF_DIR` and
`OBERTH_NONROOT_FIXTURE_DIR` to their respective scratch output directories.
Generate the isolation and WIF inputs first, then run
`TestOberth(Isolation|WIF|Nonroot)ControllerCapture`. The nonroot helper shares
the isolation capture hook and fixes the executor log level to zero.
All 24 submitted Pods passed the retained Kubernetes validator. The source PVC
references now share writable claim-level mode while the source volume mounts
remain read-only. The final
Pods replace the legacy token volume with tmpfs and isolate it from pipeline
containers. Runtime TokenRequest issuance and holder-Pod cleanup are covered
separately by unit tests and a live kind workflow.

The 2026-10-27 resource-usage wrapper recapture used the same authenticated
controller module and combined overlay. Explicit-command single-main leaves
now have the read-only measurement helper and wrapper; ContainerSets remain
unchanged. All eight fresh submitted Pods passed the retained Kubernetes
validator. This captures controller construction and mount isolation, not a
live kubelet or production cluster.

The round-25b architecture recapture used the same authenticated controller
module and combined overlay. Every wrapped container/script Pod, including
inline leaves, selects the server's arm64 architecture; unwrapped ContainerSets
remain unchanged. All eight fresh submitted Pods passed the Kubernetes validator.
The production selector uses runtime.GOARCH, not a hard-coded architecture.
On other test architectures the equality check translates only the recorded
server architecture to runtime.GOARCH. The original submitted Pods must retain
the captured architecture, and independent wrapper tests assert runtime.GOARCH.

Round 25c recaptured these eight cases with controller workflowDefaults
nodeSelector pool=ci. The architecture now enters only through the server's
strategic podSpecPatch, after Argo resolves workflow/parent scheduling selectors.
Both before-patch and submitted Pods retain pool=ci; wrapped submitted Pods
also get the server architecture. Replaying the round-25b inputs against this
controller control fails on all seven wrapped shapes, proving the regression.
All eight corrected submitted Pods pass the retained Kubernetes validator.

Round 44 (2026-10-28) recaptured current Build outputs after the 256Mi
usage instrumentation floor and server-owned admission warning/policy annotations.
The retained authenticated upstream controller and combined capture hooks ran
unchanged; all 27 submitted Pods across isolation, WIF, nonroot and VMI cases
passed the retained Kubernetes validator. Isolation inputs now explicitly declare
256Mi so the measured-leaf architecture and inherited pool checks still exercise
wrapped Pods. No expected Pod was assembled or edited by hand.

Oberth candidate (2026-11-03) recaptured current Build outputs
with explicit executor helper resources and the replay token waiter. The authenticated v4.0.8 controller ran
its real operation with retained fake-client helpers and capture-only hooks.
All 27 submitted Pods passed the retained Kubernetes v1.35.7 validator.
No expected Pod was edited manually and no live cluster was used.

The 2026-11-04 Kubernetes placement update recaptured the current Build inputs
after conservative request defaults (2 CPU and 2Gi memory where requests are
missing, honoring existing limits and template defaults). The authenticated
v4.0.8 controller ran with capture-only hooks and upstream fake clients. All
27 submitted Pods across these fixture sets passed the retained Kubernetes
v1.35.7 validator. Existing isolation, executor identity and nonroot negative
controls remain intact. No expected Pod was edited manually or live API used.

The 2026-11-04 combined resource and placement recapture uses current Build
inputs: undeclared main requests remain 2 CPU/2Gi, while repository init and
sidecar defaults are 250m/256Mi. Unpinned executor control containers retain
50m/64Mi requests with 500m/512Mi limits; pinned profiles retain their bound
executor resources. The authenticated Argo v4.0.8 controller and retained
capture helpers produced all 27 submitted Pods, which passed the retained
Kubernetes v1.35.7 validator. Pod JSON was copied directly from that controller
output, never hand-merged or assembled. No live API or credentials were used.

The 2026-11-06 helper-storage recapture uses the authenticated Argo v4.0.8
controller with capture-only overlays and fake clients. New unpinned helper
requests/limits include 256Mi/512Mi ephemeral storage; main resources, image,
credentials, mount isolation and named nonroot profile resources are preserved.
All 27 raw submitted Pods passed the retained Kubernetes v1.35.7 validator.
This is controller construction/validation evidence, not a live workload peak
or kubelet acceptance. Prior receipts are retained in the review handoff.

The 2026-11-10 release-priority recapture (#1517 review P2) regenerates the inputs after Build began setting spec.priority on release runs. Inputs are exact current Build output in the helper-storage recovery mode the regression applies to these retained historical inputs (their own v1 policy annotation). The authenticated Argo v4.0.8 module (sums equal provenance) ran its real operate(ctx) with fake clients and the retained isolation, nonroot and WIF capture helpers in one overlay (nonroot first sets klog v=0, as before; its outputs were discarded). Instrumented workflowpod.go SHA-256 e94c816c3116278d8b6c363568ab3922aed0a178e6afcd83d6fd0a3270b0e8d4 (both capture lines, tab-indented, immediately before processPodSpecPatch). Go 1.26.9 on linux/amd64, so server_goarch is amd64. Every captured Pod equals the previous capture except timestamps and the architecture value: spec.priority does not reach Pod construction. All 24 submitted Pods (16 WIF, 8 isolation) passed the retained Kubernetes v1.35.7 validator (validation-release-priority.log.txt). No Pod was edited by hand; no live cluster or credential was used.
