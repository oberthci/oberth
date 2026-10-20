package argoworkflow

// Workspace controls are the only repository-authored template annotations
// admitted. They reduce automatic workspace access and never select a Pod
// security profile, identity, or admission webhook.
const (
	WorkspaceMountsAnnotation = "oberth.ci/workspace-mounts"
	WorkspaceEnvAnnotation    = "oberth.ci/workspace-env"
)

// Values of WorkspaceMountsAnnotation. Each is a subset of the default
// (annotation absent) mounts; none of them adds a mount or authority.
//
//   - none: omit the work, persistent cache and collected-artifact mounts and
//     their injected environment.
//   - cache: like none, except the per-repository, tier-scoped persistent
//     build cache stays mounted and GOMODCACHE, GOCACHE and OBERTH_CACHE_DIR
//     point inside it, whatever workspace-env says.
//   - readonly: keep every automatic mount, read-only.
const (
	WorkspaceMountsNone     = "none"
	WorkspaceMountsCache    = "cache"
	WorkspaceMountsReadonly = "readonly"
)

// ValidWorkspaceMounts reports whether value is an admitted
// WorkspaceMountsAnnotation value.
func ValidWorkspaceMounts(value string) bool {
	switch value {
	case WorkspaceMountsNone, WorkspaceMountsCache, WorkspaceMountsReadonly:
		return true
	}
	return false
}
