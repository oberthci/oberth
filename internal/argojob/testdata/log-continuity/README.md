# Failed-init status regression

Minimized from the coordinator-provided Workflow and Pod status dumps for
2a1971bf and 105ea601 (2026-10-30). Only ownership, node identity and container
status fields remain. There are no environment, command, image, network or
credential values. Main is Waiting/PodInitializing with no container ID;
install-jq is Terminated/Error with exit 23. The preceding identity and Argo
init containers succeeded. The supplied Pod status has no termination message.

The original dump omitted Pod spec. The test supplies only the observed init
container names in a minimal spec to test the projection; this is not a captured
Pod-create spec. No log text is inferred from these status dumps.

Repository/workflow names are generic fixtures of the original byte lengths.
Node and Pod hash suffixes were recomputed with Argo's FNV-1a naming rule after
the rename, preserving exact ownership and node-binding checks. Status evidence,
UIDs, times and container outcomes are unchanged.
