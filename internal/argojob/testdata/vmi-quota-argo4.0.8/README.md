# VMI create guard controller proof

The retained Workflow is current Oberth Build output. The authenticated upstream
Argo Workflows v4.0.8 controller (commit and checksums in provenance.json)
processed it using upstream fake clients; before-patch and submitted Pods are
raw observations. The pipeline ServiceAccount deliberately has automount true.
The submitted Pod has automount false and an emissary-wrapped resource get.
It passed Kubernetes v1.35.7 ValidatePodCreate after defaulting and conversion,
using the validator retained in ../isolation-argo4.0.8/. No live cluster was used.

Regenerate in scratch directories with Go overlays, without editing the module
cache. Add generate_test.go.txt to the Oberth package as zz_vmi_generate_test.go;
set OBERTH_VMI_PROOF_DIR to this fixture output directory and run
TestExportVMIQuotaControllerInput. Add capture_test.go.txt to the authenticated
Argo workflow/controller package. As described in ../isolation-argo4.0.8/README.md,
overlay workflowpod.go with the isolationCapture callback immediately before
processPodSpecPatch. Include the existing isolation/WIF/nonroot capture helpers
if using their combined instrumented overlay. Run TestOberthVMIQuotaControllerCapture
with Go 1.27.2, then run the retained Kubernetes validator on vmi-submitted.json.
Record new provenance checksums and commit raw captures; do not edit expected
Pods to make tests pass.

TestVMICreateGuardAcceptsActualArgoPod exercises the current guard and executor
identity checks on this submitted Pod. Runtime reservation/concurrency/restart
and refusal-before-token behavior use independent API fixtures in vmi_quota_test.go.
The capture does not exercise a live KubeVirt admission webhook or guest boot.
