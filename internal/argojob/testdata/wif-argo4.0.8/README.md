# Release WIF: actual controller construction evidence

Captured with Go 1.26.6 on 2026-09-30 from authenticated
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
`97284618f06b1ca18c27a53190eeecd6704972e1364989c020a39e6e2b813dad`.
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

Use GOTOOLCHAIN=local, GOENV=off, Go 1.26.6, GOMAXPROCS=2 and
GOFLAGS='-p=2 -mod=readonly'. Overlay files are ordinary Go
`{"Replace": {"/absolute/virtual/file": "/absolute/retained/source"}}` maps.
No Docker, namespace writes, token issuance or live provider activation occurs.

The normal regression binds the installed controller version/sums, all retained
proof files and exact current Build output; it checks submitted Pod isolation
and applies today's server patch to the observed unsafe pre-patch Pod as a
negative control. These receipts do not prove kubelet rotation, IAM conditions,
STS exchange, cloud role/repository denial, or a successful keyless publisher.
Those remain separate acceptance gates before enabling WIF or retiring keys.
