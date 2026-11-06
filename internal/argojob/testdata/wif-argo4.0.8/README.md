# Release WIF: actual controller construction evidence

Recaptured with Go 1.27.2 on 2026-10-19 for #962 from authenticated
`github.com/argoproj/argo-workflows/v4 v4.0.8`, origin commit
`570c470582fc7fe41b1963d8703111679cc3d25a`.
Seven exact Oberth Build inputs each ran with the pipeline ServiceAccount's
automatic-token default both false and true (14 cases).

The upstream controller ran its real workflow operation and Pod construction
with fake API clients. A capture-only hook immediately before
`woc.processPodSpecPatch(ctx, tmpl, pod)` recorded the pre-patch Pod:

```go
if oberthWIFCapture != nil {
    oberthWIFCapture(pod.DeepCopy())
}
```

The unmodified upstream workflowpod.go SHA-256 is
`97d059a8ffed7854760fa8c109f657a4d6be0e2496ed193ee1dd541118b8f763`;
the capture-instrumented copy is
`e94c816c3116278d8b6c363568ab3922aed0a178e6afcd83d6fd0a3270b0e8d4`.
No construction behavior, input, or resulting Pod was edited.

Reproduce in a new private scratch directory (not the shared module cache):

1. Authenticate the pinned module with `go mod download -json`, compare
   module sums with provenance.json, and extract its returned Zip into scratch.
2. Set OBERTH_WIF_PROOF_DIR to an absolute scratch output directory. Use a Go
   overlay to add generate_test.go.txt as an additional internal/argojob test.
   Run `go test -overlay <overlay.json> ./internal/argojob -run
   '^TestExportWIFControllerInputs$' -count=1` from this repository.
3. Copy upstream workflow/controller/workflowpod.go into scratch, add only
   the capture hook above, and check both source digests. In another Go overlay,
   replace that source with the instrumented copy and add capture_test.go.txt as
   workflow/controller/zz_oberth_wif_capture_test.go in the extracted module.
   From that module run `go test -overlay <overlay.json> ./workflow/controller
   -run '^TestOberthWIFControllerCapture$' -count=1 -v`.
4. Use the exact validator main/module/sums retained in
   ../isolation-argo4.0.8/validation-{main.go,go.mod,go.sum}.txt. Copy them into
   an independent scratch module and run `go run . <output>/*-submitted.json`.
   This defaults, converts and validates each Pod using Kubernetes v1.35.7
   ValidatePodCreate. All 14 passed; validation.log retains the result.
5. Retain the seven generated workflows and 28 captured Pods without editing
   them. Regenerate provenance file hashes; run the normal regression test.

Use GOTOOLCHAIN=local, GOENV=off, Go 1.27.2, GOMAXPROCS=2 and
GOFLAGS='-p=2 -mod=readonly'. Overlay files are ordinary Go
`{"Replace": {"/absolute/virtual/file": "/absolute/retained/source"}}` maps.
No Docker, namespace writes, token issuance or live provider activation occurs.

The normal regression binds the installed controller version/sums, all retained
proof files and exact current Build output; it checks submitted Pod isolation
and applies today's server patch to the observed unsafe pre-patch Pod as a
negative control. These receipts do not prove kubelet rotation, IAM conditions,
STS exchange, cloud role/repository denial, or a successful keyless publisher.
Those remain separate acceptance gates before enabling WIF or retiring keys.

The 2026-10-19 recapture used a combined overlay containing the retained
isolation, WIF and nonroot capture helpers. Immediately before the controller
patch call, it invokes both `isolationCapture` and `oberthWIFCapture` when
non-nil. Set `OBERTH_ISOLATION_PROOF_DIR`, `OBERTH_WIF_PROOF_DIR` and
`OBERTH_NONROOT_FIXTURE_DIR` to their respective scratch output directories.
Generate the isolation and WIF inputs first, then run
`TestOberth(Isolation|WIF|Nonroot)ControllerCapture`. The nonroot helper shares
the isolation capture hook and fixes the executor log level to zero.
All 24 submitted Pods passed the retained Kubernetes validator. The source PVC
references now share writable claim-level mode while source mounts remain
read-only. The final
Pods replace the legacy token volume with tmpfs and isolate it from pipeline
containers. Runtime TokenRequest issuance and holder-Pod cleanup are covered
separately by unit tests and a live kind workflow.

Round 26 adds the wif-only-step case with an administrator-defined role and
a dedicated step account. The input has no secret grants or secret-derived
repository identity; ordinary workflow identity is the uncredentialed pipeline
account. The actual Argo v4.0.8 controller selects release-step-main for the
Pod, keeps automount false even when that account defaults true, and preserves
the existing main-only WIF/executor isolation. Both new submitted Pods pass
Kubernetes v1.35.7 validation. Legacy cases and their input/output captures are
retained unchanged. This proves Pod construction, not external cloud IAM.

Round 26b recaptures only the five wif-only-step workflow/Pod files. That input
now uses AdmitRelease's bare tag spelling and an exact administrator file/pattern
identity pin. The policy digest includes that pin. The other seven inputs and
retained captures are unchanged. All sixteen newly constructed submitted Pods
were checked with the same Kubernetes v1.35.7 validator. Cloud IAM remains a
separate acceptance gate.

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
