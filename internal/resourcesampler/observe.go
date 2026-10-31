package resourcesampler

import (
	"math"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/internal/model"
)

// Binding is supplied only after the controller verifies Workflow ownership and
// resolves this Pod node through the admitted Workflow's step tree.
type Binding struct {
	RunID, WorkflowName, WorkflowUID, Namespace, NodeID, NodeName, PodName, Burn, Step string
}

// Observe does not trust metric labels alone: UID, current container incarnation
// and the kubelet cgroup path must agree. CPU/memory/IO are main-container scope;
// ephemeral storage and PSI are pod scope (including helpers), not PVC/VM disks.
func Observe(b Binding, pod *corev1.Pod, snapshot Snapshot, now time.Time) (model.StepResourceUsage, bool) {
	if pod == nil || string(pod.UID) == "" || pod.Namespace != b.Namespace || pod.Name != b.PodName || pod.Spec.NodeName == "" || pod.Annotations["workflows.argoproj.io/node-id"] != b.NodeID || pod.Annotations["workflows.argoproj.io/node-name"] != b.NodeName {
		return model.StepResourceUsage{}, false
	}
	owners := 0
	for _, owner := range pod.OwnerReferences {
		if owner.Controller != nil && *owner.Controller {
			owners++
			if owner.APIVersion != "argoproj.io/v1alpha1" || owner.Kind != "Workflow" || owner.Name != b.WorkflowName || owner.UID != types.UID(b.WorkflowUID) {
				return model.StepResourceUsage{}, false
			}
		}
	}
	if owners != 1 {
		return model.StepResourceUsage{}, false
	}
	var status *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == "main" {
			if status != nil {
				return model.StepResourceUsage{}, false
			}
			status = &pod.Status.ContainerStatuses[i]
		}
	}
	if status == nil || status.ContainerID == "" || (status.State.Running == nil && status.State.Terminated == nil) {
		return model.StepResourceUsage{}, false
	}
	v := model.StepResourceUsage{Version: 1, Source: "kubelet-cadvisor", RunID: b.RunID, WorkflowName: b.WorkflowName, WorkflowUID: b.WorkflowUID, Namespace: b.Namespace, NodeID: b.NodeID, PodName: pod.Name, PodUID: string(pod.UID), NodeName: pod.Spec.NodeName, ContainerID: status.ContainerID, ContainerName: "main", RestartCount: status.RestartCount, Burn: b.Burn, Step: b.Step, FirstObservedAt: now, LastObservedAt: now, SampledLowerBound: true}
	for _, container := range pod.Spec.Containers {
		if container.Name == "main" {
			if q, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
				v.CPURequestNanoCores = number(q.AsApproximateFloat64()*1e9, &v.Saturated)
			}
			if q, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
				v.MemoryRequestBytes = number(q.AsApproximateFloat64(), &v.Saturated)
			}
		}
	}
	// An absent Kubernetes request schedules as zero. That known zero differs
	// from an absent measurement and is never used as a ratio denominator.
	if v.CPURequestNanoCores == nil {
		v.CPURequestNanoCores = int64ptr(0)
	}
	if v.MemoryRequestBytes == nil {
		v.MemoryRequestBytes = int64ptr(0)
	}
	v.EphemeralRequestBytes = number(ephemeralRequest(pod), &v.Saturated)
	if term := status.State.Terminated; term != nil {
		if term.ContainerID != "" && term.ContainerID != status.ContainerID {
			return model.StepResourceUsage{}, false
		}
		code := term.ExitCode
		v.Terminal = true
		v.ExitCode = &code
		switch term.Reason {
		case "Completed", "Error", "OOMKilled":
			v.Reason = term.Reason
		default:
			v.Reason = "other"
		}
	}
	devices := map[string]bool{}
	if snapshot.MetricsOK {
		for _, metric := range snapshot.Metrics {
			l := metric.Labels
			if l["namespace"] != pod.Namespace || l["pod"] != pod.Name || l["container"] != "main" || !fresh(metric.At, now) || !CgroupMatches(l["id"], string(pod.UID), status.ContainerID) {
				continue
			}
			if metric.At.After(v.ContainerSourceAt) {
				v.ContainerSourceAt = metric.At
			}
			if metric.At.After(v.SourceAt) {
				v.SourceAt = metric.At
			}
			n := number(metric.Value, &v.Saturated)
			switch metric.Name {
			case "container_memory_usage_bytes", "container_memory_max_usage_bytes":
				maximum(&v.MemoryPeakBytes, n)
			case "container_cpu_usage_seconds_total":
				if l["cpu"] != "total" {
					continue
				}
				if metric.At.After(v.CPUTimeAt) {
					v.CPUTimeAt = metric.At
					v.CPUTimeNS = number(metric.Value*1e9, &v.Saturated)
				}
			case "container_fs_reads_bytes_total", "container_fs_writes_bytes_total", "container_fs_reads_total", "container_fs_writes_total":
				device := l["device"]
				key := metric.Name + "\x00" + device
				if device == "" || device == "total" || devices[key] {
					continue
				}
				devices[key] = true
				var target **int64
				switch metric.Name {
				case "container_fs_reads_bytes_total":
					target = &v.IOReadBytes
				case "container_fs_writes_bytes_total":
					target = &v.IOWriteBytes
				case "container_fs_reads_total":
					target = &v.IOReadOps
				default:
					target = &v.IOWriteOps
				}
				add(target, n, &v.Saturated)
			}
		}
	}
	if snapshot.SummaryOK {
		for _, stats := range snapshot.Summary.Pods {
			if stats.PodRef.Namespace != pod.Namespace || stats.PodRef.Name != pod.Name || stats.PodRef.UID != string(pod.UID) {
				continue
			}
			if fs := stats.EphemeralStorage; fs != nil && fs.UsedBytes != nil && fresh(fs.Time, now) {
				v.EphemeralStoragePeakBytes = unsigned(*fs.UsedBytes, &v.Saturated)
				if fs.Time.After(v.SourceAt) {
					v.SourceAt = fs.Time
				}
			}
			if io := stats.IO; io != nil && io.PSI != nil && fresh(io.Time, now) {
				v.IOPressureAt = io.Time
				if io.PSI.Some.Total != nil {
					v.IOPressureSomeNS = unsigned(*io.PSI.Some.Total, &v.Saturated)
				}
				if io.PSI.Full.Total != nil {
					v.IOPressureFullNS = unsigned(*io.PSI.Full.Total, &v.Saturated)
				}
				if io.Time.After(v.SourceAt) {
					v.SourceAt = io.Time
				}
			}
			break
		}
	}
	if !v.SourceAt.IsZero() {
		v.Samples = 1
	}
	if v.Samples == 0 && !v.Terminal {
		return model.StepResourceUsage{}, false
	}
	return v, v.Validate() == nil
}

func int64ptr(v int64) *int64 { return &v }
func number(n float64, saturated *bool) *int64 {
	if math.IsNaN(n) || n < 0 {
		return nil
	}
	if math.IsInf(n, 1) || n >= float64(math.MaxInt64) {
		*saturated = true
		return int64ptr(math.MaxInt64)
	}
	return int64ptr(int64(n))
}
func unsigned(n uint64, saturated *bool) *int64 {
	if n > math.MaxInt64 {
		*saturated = true
		return int64ptr(math.MaxInt64)
	}
	return int64ptr(int64(n))
}
func maximum(target **int64, n *int64) {
	if n != nil && (*target == nil || *n > **target) {
		*target = n
	}
}
func add(target **int64, n *int64, saturated *bool) {
	if n == nil {
		return
	}
	if *target == nil {
		*target = int64ptr(0)
	}
	if **target > math.MaxInt64-*n {
		**target = math.MaxInt64
		*saturated = true
	} else {
		**target += *n
	}
}
func ephemeralRequest(pod *corev1.Pod) float64 {
	sum := 0.0
	for _, c := range pod.Spec.Containers {
		q := c.Resources.Requests[corev1.ResourceEphemeralStorage]
		sum += q.AsApproximateFloat64()
	}
	initPeak, restartable := 0.0, 0.0
	for _, c := range pod.Spec.InitContainers {
		q := c.Resources.Requests[corev1.ResourceEphemeralStorage]
		n := q.AsApproximateFloat64()
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			restartable += n
			initPeak = math.Max(initPeak, restartable)
		} else {
			initPeak = math.Max(initPeak, restartable+n)
		}
	}
	q := pod.Spec.Overhead[corev1.ResourceEphemeralStorage]
	return math.Max(sum+restartable, initPeak) + q.AsApproximateFloat64()
}

// Current cAdvisor cgroup paths use the full runtime container ID, never a
// user-selected short prefix or a pod name reused by a later UID.
func validContainerID(id string) bool {
	runtime, cid, ok := strings.Cut(id, "://")
	if !ok || (runtime != "containerd" && runtime != "cri-o" && runtime != "docker") || len(cid) != 64 {
		return false
	}
	for _, ch := range cid {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
