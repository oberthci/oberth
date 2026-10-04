# Controller observations

These fixtures were captured from the authenticated Argo Workflows v4.0.8
module. `provenance.json` pins module sums, the controller source digest and
each JSON file. The upstream fake controller used its existing service-account
token fixture and intentionally hostile executor security defaults. The pre-patch `exec-sa-token` Secret reference comes from the upstream
fixture. The current server patch replaces it with tmpfs; the submitted Pods
contain no token Secret. A fixed initializer waits for stdin-only delivery of
a short-lived token bound to a separate, unscheduled executor identity Pod.

`*-before-patch.json` records `workflow/controller/workflowpod.go` immediately
before `var podSpecPatchs` and `processPodSpecPatch` (around line 405).
`*-submitted.json` records the Pod fetched from the fake Kubernetes client
after the real constructor submitted it. No final fixture was hand assembled.

The ordinary regression reconstructs CURRENT Build-owned main container,
template JSON, selected source/scratch volumes and identities in the raw Pod.
It preserves controller-owned executor/token fields, rebuilds main filesystem
mirrors as the controller does before patching, then applies the CURRENT
server patch with Kubernetes strategic merge. It models only these subsequent
v4.0.8 mutations at workflowpod.go:417–456, in order:

1. Wrap the main command with the executor's emissary command and fixed log flags.
2. Mount the wait executor's tmp-dir-argo at `/tmp`, using container index 0
   as the kubelet-facing subpath.
3. Append the shared `/var/run/argo` mount to main and wait.
4. Apply workflow Pod labels through the template-resolution helper.

For scripts, the controller's earlier `addScriptStagingVolume` and
`executeScript` argument append are included before the patch. The entire
result must equal the actual submitted observation. Unexpected controller
shapes, termination-grace mutation and template/argument offload fail rather
than silently expanding the model. Version/checksum guards prevent a dependency
upgrade from silently reusing these semantics.

The full local controller proof covered both shapes, executor context
replacement, init ordering, token placement, scratch subpaths and read-only
input mirrors. Ordinary tests retain the current Build/patch comparison and
negative controls; they do not execute the upstream constructor. The local
capture tool's separate vulnerable dependency closure is intentionally absent
from the shipped tree. Fresh controller capture and deployment-specific
executor compatibility remain required before enabling consumers.

The current capture additionally runs the exact built-in public profile and
pinned executor image with gloglevel0, separately from the hostile-context
control. It retains the first pre-patch Pod snapshot even when later actual
reconciliation requests another Pod. The real controller `operate` method
persists a Running Workflow with nodes and an unchanged admitted spec;
`*-persisted-workflow.json` preserves that observed object for recovery tests.
Metadata/status changes are accepted, while policy/template/volume changes are
rejected without loosening the spec comparison.

`controller-profile.json` is an actual Helm4.2.3 render of authenticated upstream
chart1.0.24, SHA2567b1d540095ba32bb8c914432b554c64809b47a6f737b1448273d3a4740be3d50,
using the current installer's profile values and normal flags. It contains only
public configuration and a controller Pod template. The verifier regression
adds fixture identity/status and the enumerated standard API service-account
projection; no token values or live-cluster objects are read.

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
