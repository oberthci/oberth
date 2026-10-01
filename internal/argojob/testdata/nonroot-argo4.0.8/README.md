# Controller observations

These fixtures were captured from the authenticated Argo Workflows v4.0.8
module. `provenance.json` pins module sums, the controller source digest and
each JSON file. The upstream fake controller used its existing service-account
token fixture and intentionally hostile executor security defaults. The
`exec-sa-token` Secret reference is inherited fixture infrastructure, contains
no secret value, and was not introduced by this Oberth change.

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
