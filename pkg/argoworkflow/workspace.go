package argoworkflow

// Workspace controls are the only repository-authored template annotations
// admitted. They reduce automatic workspace access and never select a Pod
// security profile, identity, or admission webhook.
const (
	WorkspaceMountsAnnotation = "oberth.ci/workspace-mounts"
	WorkspaceEnvAnnotation    = "oberth.ci/workspace-env"
)
