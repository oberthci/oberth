// Package podpriority names the chart-owned scheduling tiers.
package podpriority

const ControlPlaneValue int32 = 1000000
const PipelineValue int32 = 0

func ControlPlaneClass(namespace, release string) string {
	return namespace + "-" + release + "-control-plane"
}

// A pipeline namespace has one execution owner, independently of its server's
// namespace or Helm release name. Remote targets use their own namespace tier.
func PipelineClass(namespace string) string { return namespace + "-oberth-pipeline" }
