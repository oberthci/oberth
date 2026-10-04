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
Do not edit expected Pods manually to make tests pass. Use `/var/tmp` for build
scratch and cap Go concurrency; no live API or module-cache edits are needed.

The normal regression checks that current Build output still equals the
captured input, checks the submitted mounts/identity/environment, and applies
the current server patch to the observed writable mirrors as a negative-control
boundary test. A changed input requires fresh controller proof. This is not a
test of custom cluster admission webhooks, image execution, or artifact fetches.

The 2026-10-04 recapture used a combined overlay containing the retained
isolation, WIF and nonroot capture helpers. Immediately before the controller
patch call, it invokes both `isolationCapture` and `oberthWIFCapture` when
non-nil. Set `OBERTH_ISOLATION_PROOF_DIR`, `OBERTH_WIF_PROOF_DIR` and
`OBERTH_NONROOT_FIXTURE_DIR` to their respective scratch output directories.
Generate the isolation and WIF inputs first, then run
`TestOberth(Isolation|WIF|Nonroot)ControllerCapture`. The nonroot helper shares
the isolation capture hook and fixes the executor log level to zero.
All 24 submitted Pods passed the retained Kubernetes validator. The final
Pods replace the legacy token volume with tmpfs and isolate it from pipeline
containers. Runtime TokenRequest issuance and holder-Pod cleanup are covered
separately by unit tests and a live kind workflow.
