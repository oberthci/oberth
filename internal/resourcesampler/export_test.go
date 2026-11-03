package resourcesampler

import (
	"time"

	"github.com/oberthci/oberth/internal/model"
	corev1 "k8s.io/api/core/v1"
)

// Observe does not trust metric labels alone: UID, current container incarnation
// and the kubelet cgroup path must agree. CPU/memory/IO are main-container scope;
// ephemeral storage and PSI are pod scope (including helpers), not PVC/VM disks.
func Observe(b Binding, pod *corev1.Pod, snapshot Snapshot, now time.Time) (model.StepResourceUsage, bool) {
	v, ok := observe(b, pod, IndexSnapshot(snapshot), now, "main", "app", "workflow", false)
	return v, ok && (v.Samples > 0 || v.Terminal)
}
