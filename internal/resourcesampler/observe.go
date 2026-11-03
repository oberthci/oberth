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
	TemplateName, FragmentRepository                                                   string
	RunID, WorkflowName, WorkflowUID, Namespace, NodeID, NodeName, PodName, Burn, Step string
	LauncherOwnerName, LauncherOwnerUID                                                string
}

// Observe does not trust metric labels alone: UID, current container incarnation
// and the kubelet cgroup path must agree. CPU/memory/IO are main-container scope;
// ephemeral storage and PSI are pod scope (including helpers), not PVC/VM disks.
func Observe(b Binding, pod *corev1.Pod, snapshot Snapshot, now time.Time) (model.StepResourceUsage, bool) {
	v, ok := observe(b, pod, IndexSnapshot(snapshot), now, "main", "app", "workflow", false)
	return v, ok && (v.Samples > 0 || v.Terminal)
}

// ObserveContainers covers app containers (including ContainerSets and Argo
// wait), ordinary/restartable init helpers, and ephemeral containers. All use
// the same UID, full runtime ID, freshness and cgroup checks as main.
func ObserveContainers(b Binding, pod *corev1.Pod, snapshot Snapshot, now time.Time) []model.StepResourceUsage {
	return observeContainers(b, pod, snapshot, now, "workflow", false)
}

// ObserveLauncher is called only after the controller verifies the complete
// Workflow -> VMI -> Pod owner chain by UID. It measures launcher totals;
// kubelet data cannot separate guest consumption from hypervisor overhead.
func ObserveLauncher(b Binding, pod *corev1.Pod, snapshot Snapshot, now time.Time) []model.StepResourceUsage {
	return observeContainers(b, pod, snapshot, now, "vm-launcher", true)
}
func observeContainers(b Binding, pod *corev1.Pod, snapshot Snapshot, now time.Time, podKind string, launcher bool) []model.StepResourceUsage {
	if pod == nil || len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses)+len(pod.Status.EphemeralContainerStatuses) > 128 || len(pod.Spec.Containers)+len(pod.Spec.InitContainers)+len(pod.Spec.EphemeralContainers) > 128 {
		return nil
	}
	snapshot = IndexSnapshot(snapshot)
	out := []model.StepResourceUsage{}
	for _, group := range []struct {
		kind     string
		statuses []corev1.ContainerStatus
	}{{"app", pod.Status.ContainerStatuses}, {"init", pod.Status.InitContainerStatuses}, {"ephemeral", pod.Status.EphemeralContainerStatuses}} {
		for _, status := range group.statuses {
			if v, ok := observe(b, pod, snapshot, now, status.Name, group.kind, podKind, launcher); ok {
				out = append(out, v)
			}
		}
	}
	return out
}
func observe(b Binding, pod *corev1.Pod, snapshot Snapshot, now time.Time, name, kind, podKind string, launcher bool) (model.StepResourceUsage, bool) {
	if pod == nil || string(pod.UID) == "" || pod.Namespace != b.Namespace || pod.Name != b.PodName || pod.Spec.NodeName == "" || (!launcher && (pod.Annotations["workflows.argoproj.io/node-id"] != b.NodeID || pod.Annotations["workflows.argoproj.io/node-name"] != b.NodeName)) {
		return model.StepResourceUsage{}, false
	}
	owners := 0
	for _, owner := range pod.OwnerReferences {
		if owner.Controller != nil && *owner.Controller {
			owners++
			if launcher && (b.LauncherOwnerUID == "" || owner.APIVersion != "kubevirt.io/v1" || owner.Kind != "VirtualMachineInstance" || owner.Name != b.LauncherOwnerName || string(owner.UID) != b.LauncherOwnerUID) {
				return model.StepResourceUsage{}, false
			}
			if !launcher && (owner.APIVersion != "argoproj.io/v1alpha1" || owner.Kind != "Workflow" || owner.Name != b.WorkflowName || owner.UID != types.UID(b.WorkflowUID)) {
				return model.StepResourceUsage{}, false
			}
		}
	}
	if owners != 1 {
		return model.StepResourceUsage{}, false
	}
	statuses := pod.Status.ContainerStatuses
	containers := pod.Spec.Containers
	if kind == "init" {
		statuses = pod.Status.InitContainerStatuses
		containers = pod.Spec.InitContainers
	}
	if kind == "ephemeral" {
		statuses = pod.Status.EphemeralContainerStatuses
		containers = nil
		for _, c := range pod.Spec.EphemeralContainers {
			containers = append(containers, corev1.Container{Name: c.Name, Resources: c.Resources})
		}
	}
	var status *corev1.ContainerStatus
	for i := range statuses {
		if statuses[i].Name == name {
			if status != nil {
				return model.StepResourceUsage{}, false
			}
			status = &statuses[i]
		}
	}
	if status == nil || status.ContainerID == "" || (status.State.Running == nil && status.State.Terminated == nil) {
		return model.StepResourceUsage{}, false
	}
	v := model.StepResourceUsage{Version: 2, ContainerKind: kind, PodKind: podKind, Source: "kubelet-cadvisor", RunID: b.RunID, WorkflowName: b.WorkflowName, WorkflowUID: b.WorkflowUID, Namespace: b.Namespace, NodeID: b.NodeID, PodName: pod.Name, PodUID: string(pod.UID), NodeName: pod.Spec.NodeName, ContainerID: status.ContainerID, ContainerName: name, RestartCount: status.RestartCount, Burn: b.Burn, Step: b.Step, FirstObservedAt: now, LastObservedAt: now, SampledLowerBound: true}
	v.TemplateName, v.FragmentRepository = b.TemplateName, b.FragmentRepository
	declared := 0
	for _, container := range containers {
		if container.Name == name {
			declared++
			v.CPULimitNanoCores = int64ptr(0)
			v.MemoryLimitBytes = int64ptr(0)
			v.EphemeralLimitBytes = int64ptr(0)
			if q, ok := container.Resources.Limits[corev1.ResourceCPU]; ok {
				v.CPULimitNanoCores = number(q.AsApproximateFloat64()*1e9, &v.Saturated)
			}
			if q, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
				v.MemoryLimitBytes = number(q.AsApproximateFloat64(), &v.Saturated)
			}
			if q, ok := container.Resources.Limits[corev1.ResourceEphemeralStorage]; ok {
				v.EphemeralLimitBytes = number(q.AsApproximateFloat64(), &v.Saturated)
			}
			if q, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
				v.CPURequestNanoCores = number(q.AsApproximateFloat64()*1e9, &v.Saturated)
			}
			if q, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
				v.MemoryRequestBytes = number(q.AsApproximateFloat64(), &v.Saturated)
			}
		}
	}
	if declared != 1 {
		return model.StepResourceUsage{}, false
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
	v.PodCreatedAt = pod.CreationTimestamp.Time
	if pod.Status.StartTime != nil {
		v.PodStartedAt = pod.Status.StartTime.Time
	}
	if running := status.State.Running; running != nil {
		v.ContainerStartedAt = running.StartedAt.Time
	}
	v.PodFinishedAt = PodFinishedAt(pod)
	if term := status.State.Terminated; term != nil {
		if term.ContainerID != "" && term.ContainerID != status.ContainerID {
			return model.StepResourceUsage{}, false
		}
		v.ContainerStartedAt = term.StartedAt.Time
		if !v.ContainerStartedAt.IsZero() {
			v.ContainerFinishedAt = term.FinishedAt.Time
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
		for _, metric := range snapshot.index.metrics[pod.Namespace+"\x00"+pod.Name+"\x00"+name] {
			l := metric.Labels
			if l["namespace"] != pod.Namespace || l["pod"] != pod.Name || l["container"] != name || !fresh(metric.At, now) || !CgroupMatches(l["id"], string(pod.UID), status.ContainerID) {
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
		for _, stats := range []PodStats{snapshot.index.pods[string(pod.UID)]} {
			if stats.PodRef.Namespace != pod.Namespace || stats.PodRef.Name != pod.Name || stats.PodRef.UID != string(pod.UID) {
				continue
			}
			if cpu := stats.CPU; cpu != nil && cpu.UsageNanoCores != nil && fresh(cpu.Time, now) {
				v.PodCPUPeakNanoCores = unsigned(*cpu.UsageNanoCores, &v.Saturated)
				if cpu.Time.After(v.SourceAt) {
					v.SourceAt = cpu.Time
				}
			}
			if mem := stats.Memory; mem != nil && mem.UsageBytes != nil && fresh(mem.Time, now) {
				v.PodMemoryPeakBytes = unsigned(*mem.UsageBytes, &v.Saturated)
				if mem.Time.After(v.SourceAt) {
					v.SourceAt = mem.Time
				}
			}
			// Summary startTime is CRI/cAdvisor creation time, not running start.
			// Bound it between the previous finish and this incarnation's start.
			containerStats := stats.Containers
			if len(containerStats) > 128 {
				containerStats = nil
			}
			for _, cs := range containerStats {
				if cs.Name != name || !summaryIncarnationMatches(cs.StartTime, status, v.ContainerStartedAt) {
					continue
				}
				for _, fs := range []struct {
					src *FSStats
					dst **int64
				}{{cs.Rootfs, &v.RootfsPeakBytes}, {cs.Logs, &v.LogsPeakBytes}} {
					if fs.src != nil && fs.src.UsedBytes != nil && fresh(fs.src.Time, now) && !fs.src.Time.Before(v.ContainerStartedAt) {
						*fs.dst = unsigned(*fs.src.UsedBytes, &v.Saturated)
						if fs.src.Time.After(v.SourceAt) {
							v.SourceAt = fs.src.Time
						}
					}
				}
			}
			if used, at := pvcUsage(pod, stats, now, &v.Saturated); used != nil {
				v.PodPVCUsedPeakBytes = used
				if at.After(v.SourceAt) {
					v.SourceAt = at
				}
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
	if v.Samples == 0 && !v.Terminal && v.ContainerStartedAt.IsZero() {
		return model.StepResourceUsage{}, false
	}
	// A skewed node clock must not discard otherwise valid usage. Only timing
	// fields beyond the server's tolerance become unknown.
	for _, at := range []*time.Time{&v.PodCreatedAt, &v.PodStartedAt, &v.PodFinishedAt, &v.ContainerStartedAt, &v.ContainerFinishedAt} {
		if at.After(now.Add(2 * time.Second)) {
			*at = time.Time{}
		}
	}
	if v.ContainerStartedAt.IsZero() {
		v.ContainerFinishedAt = time.Time{}
	}
	if v.PodStartedAt.IsZero() {
		v.PodFinishedAt = time.Time{}
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

// PodFinishedAt returns a finish only when terminal phase and every declared
// container status prove it. The controller uses it for one late timing update.
func PodFinishedAt(pod *corev1.Pod) time.Time {
	if pod.Status.StartTime == nil || (pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed) || len(pod.Status.ContainerStatuses) != len(pod.Spec.Containers) || len(pod.Status.InitContainerStatuses) != len(pod.Spec.InitContainers) || len(pod.Status.EphemeralContainerStatuses) != len(pod.Spec.EphemeralContainers) {
		return time.Time{}
	}
	var end time.Time
	for _, group := range [][]corev1.ContainerStatus{pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses} {
		for _, status := range group {
			if status.State.Terminated == nil || status.State.Terminated.FinishedAt.IsZero() {
				return time.Time{}
			}
			if status.State.Terminated.FinishedAt.After(end) {
				end = status.State.Terminated.FinishedAt.Time
			}
		}
	}
	return end
}

func pvcUsage(pod *corev1.Pod, stats PodStats, now time.Time, saturated *bool) (*int64, time.Time) {
	if len(pod.Spec.Volumes) > 128 || len(stats.Volumes) > 128 {
		return nil, time.Time{}
	}
	claims := map[string]string{}
	for _, volume := range pod.Spec.Volumes {
		if volume.Ephemeral != nil {
			return nil, time.Time{}
		}
		if volume.PersistentVolumeClaim != nil {
			claims[volume.Name] = volume.PersistentVolumeClaim.ClaimName
		}
	}
	if len(claims) == 0 {
		return nil, time.Time{}
	}
	seen := map[string]bool{}
	var used *int64
	var at time.Time
	for _, claim := range claims {
		if seen[claim] {
			continue
		}
		found := false
		for _, v := range stats.Volumes {
			if claims[v.Name] != claim || v.PVCRef == nil || v.PVCRef.Name != claim || v.PVCRef.Namespace != pod.Namespace || v.UsedBytes == nil || !fresh(v.Time, now) {
				continue
			}
			// Do not invent a simultaneous total from independently timed peaks.
			if !at.IsZero() && !v.Time.Equal(at) {
				return nil, time.Time{}
			}
			add(&used, unsigned(*v.UsedBytes, saturated), saturated)
			at = v.Time
			found = true
			break
		}
		if !found {
			return nil, time.Time{}
		}
		seen[claim] = true
	}
	return used, at
}

// Kubelet v1.36.5 cri_stats_provider.go uses container.CreatedAt for summary
// startTime; the cAdvisor provider uses info.Spec.CreationTime. Both precede
// Kubernetes Running.StartedAt. On restart, only a known previous finish can
// exclude the old incarnation's name-only summary entry.
func summaryIncarnationMatches(created time.Time, status *corev1.ContainerStatus, started time.Time) bool {
	if created.IsZero() || started.IsZero() || created.After(started) {
		return false
	}
	if status.RestartCount == 0 {
		return true
	}
	previous := status.LastTerminationState.Terminated
	return previous != nil && !previous.FinishedAt.IsZero() && !created.Before(previous.FinishedAt.Time)
}
