package resourcesampler

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
)

func sampleFixture(t *testing.T) (Binding, *corev1.Pod, Snapshot, time.Time) {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Millisecond)
	b := Binding{RunID: strings.Repeat("a", 32), WorkflowName: "workflow-test", WorkflowUID: "11111111-1111-4111-8111-111111111111", Namespace: "argo", NodeID: "workflow-test-123", NodeName: "workflow-test.test", PodName: "workflow-test-123", Burn: "checks", Step: "test"}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: b.PodName, Namespace: b.Namespace, UID: "22222222-2222-4222-8222-222222222222", Annotations: map[string]string{"workflows.argoproj.io/node-id": b.NodeID, "workflows.argoproj.io/node-name": b.NodeName}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: b.WorkflowName, UID: "11111111-1111-4111-8111-111111111111", Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "main", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}}}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "main", ContainerID: "containerd://" + strings.Repeat("b", 64), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(at.Add(-time.Minute))}}}}}}
	id := "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + strings.ReplaceAll(string(p.UID), "-", "_") + ".slice/cri-containerd-" + strings.Repeat("b", 64) + ".scope"
	raw := ""
	for name, value := range map[string]float64{"container_memory_usage_bytes": 123456, "container_cpu_usage_seconds_total": 2, "container_fs_reads_bytes_total": 1000, "container_fs_writes_bytes_total": 2000, "container_fs_reads_total": 3, "container_fs_writes_total": 4} {
		raw += fmt.Sprintf("%s{namespace=%q,pod=%q,container=\"main\",id=%q,cpu=\"total\",device=\"nvme0n1\"} %g %d\n", name, p.Namespace, p.Name, id, value, at.UnixMilli())
	}
	metrics, err := ParseMetrics([]byte(raw))
	if err != nil || len(metrics) != 6 {
		t.Fatalf("metrics %d %v", len(metrics), err)
	}
	return b, p, Snapshot{MetricsOK: true, Metrics: metrics}, at
}
func TestResourceObservationTrustedBindingAndKilledRetention(t *testing.T) {
	b, p, s, at := sampleFixture(t)
	v, ok := Observe(b, p, s, at)
	if !ok || v.MemoryPeakBytes == nil || *v.MemoryPeakBytes != 123456 || *v.CPURequestNanoCores != 500000000 || *v.IOReadBytes != 1000 || v.CPUPeakNanoCores != nil || v.Validate() != nil {
		t.Fatalf("observation %#v", v)
	}
	for i := range s.Metrics {
		s.Metrics[i].At = at.Add(5 * time.Second)
		if s.Metrics[i].Name == "container_cpu_usage_seconds_total" {
			s.Metrics[i].Value = 7
		}
		if s.Metrics[i].Name == "container_fs_reads_bytes_total" {
			s.Metrics[i].Value = 1500
		}
	}
	next, ok := Observe(b, p, s, at.Add(5*time.Second))
	if !ok {
		t.Fatal("second observation missing")
	}
	merged, err := model.MergeResourceUsage(v, next)
	if err != nil || merged.CPUPeakNanoCores == nil || *merged.CPUPeakNanoCores != 1000000000 || *merged.IOReadBytes != 1500 || merged.Samples != 2 {
		t.Fatalf("merged %#v %v", merged, err)
	}
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled", ContainerID: p.Status.ContainerStatuses[0].ContainerID}}
	killed, ok := Observe(b, p, Snapshot{}, at.Add(6*time.Second))
	if !ok || killed.Samples != 0 || killed.MemoryPeakBytes != nil {
		t.Fatalf("unknown killed %#v", killed)
	}
	merged, err = model.MergeResourceUsage(merged, killed)
	if err != nil || !merged.Terminal || merged.Reason != "OOMKilled" || *merged.MemoryPeakBytes != 123456 || *merged.IOReadBytes != 1500 || merged.Samples != 2 {
		t.Fatalf("killed loses prior sample %#v %v", merged, err)
	}
}
func TestResourceObservationRejectsForeignStaleAndFabricatedMetrics(t *testing.T) {
	for _, kind := range []string{"owner", "uid-path", "cid-path", "metric-namespace", "metric-container", "timestamp", "future", "no-timestamp", "last-state", "duplicate-main", "termination-id"} {
		t.Run(kind, func(t *testing.T) {
			b, p, s, at := sampleFixture(t)
			for i := range s.Metrics {
				switch kind {
				case "uid-path":
					s.Metrics[i].Labels["id"] = strings.ReplaceAll(s.Metrics[i].Labels["id"], "22222222", "33333333")
				case "cid-path":
					s.Metrics[i].Labels["id"] += "fake"
				case "metric-namespace":
					s.Metrics[i].Labels["namespace"] = "other"
				case "metric-container":
					s.Metrics[i].Labels["container"] = "wait"
				case "timestamp":
					s.Metrics[i].At = at.Add(-time.Minute)
				case "future":
					s.Metrics[i].At = at.Add(time.Minute)
				case "no-timestamp":
					s.Metrics[i].At = time.Time{}
				}
			}
			switch kind {
			case "owner":
				p.OwnerReferences[0].UID = "foreign"
			case "last-state":
				p.Status.ContainerStatuses[0].LastTerminationState = p.Status.ContainerStatuses[0].State
				p.Status.ContainerStatuses[0].State = corev1.ContainerState{}
			case "duplicate-main":
				p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, p.Status.ContainerStatuses[0])
			case "termination-id":
				p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: "containerd://" + strings.Repeat("c", 64)}}
			}
			if v, ok := Observe(b, p, s, at); ok {
				t.Fatalf("accepted %s %#v", kind, v)
			}
		})
	}
}
func TestResourceParserBoundsAndMandatoryTimestamp(t *testing.T) {
	for _, raw := range []string{`container_memory_usage_bytes{container="main"} NaN 10`, `container_memory_usage_bytes{container="main",container="main"} 1 10`, `container_memory_usage_bytes{container="main"} 1 nope`, strings.Repeat("x", MaxResponseBytes+1)} {
		if _, err := ParseMetrics([]byte(raw)); err == nil {
			t.Fatal("invalid exposition accepted")
		}
	}
	metrics, err := ParseMetrics([]byte("container_memory_usage_bytes{container=\"main\"} 99\ncontainer_last_seen{container=\"main\"} 999 100\n"))
	if err != nil || len(metrics) != 0 {
		t.Fatal("scrape time accepted as sample time")
	}
	if CgroupMatches("/pod22222222-2222-4222-8222-222222222222/"+strings.Repeat("g", 64), "22222222-2222-4222-8222-222222222222", "containerd://"+strings.Repeat("g", 64)) {
		t.Fatal("non-hex cid accepted")
	}
}

func TestResourceMergeIndependentSummaryAndCadvisorClocks(t *testing.T) {
	b, p, s, at := sampleFixture(t)
	raw := fmt.Sprintf(`{"node":{"nodeName":"node-a"},"pods":[{"podRef":{"name":%q,"namespace":%q,"uid":%q},"ephemeral-storage":{"time":%q,"usedBytes":42}}]}`, p.Name, p.Namespace, p.UID, at.Add(2*time.Second).Format(time.RFC3339Nano))
	if err := json.Unmarshal([]byte(raw), &s.Summary); err != nil {
		t.Fatal(err)
	}
	s.SummaryOK = true
	first, ok := Observe(b, p, s, at)
	if !ok {
		t.Fatal("first sample missing")
	}
	for i := range s.Metrics {
		s.Metrics[i].At = at.Add(time.Second)
		switch s.Metrics[i].Name {
		case "container_memory_usage_bytes":
			s.Metrics[i].Value = 200000
		case "container_cpu_usage_seconds_total":
			s.Metrics[i].Value = 3
		case "container_fs_reads_bytes_total":
			s.Metrics[i].Value = 500
		}
	}
	next, ok := Observe(b, p, s, at.Add(time.Second))
	if !ok {
		t.Fatal("second sample missing")
	}
	if !first.SourceAt.Equal(next.SourceAt) {
		t.Fatal("fixture does not pin independent clocks")
	}
	merged, err := model.MergeResourceUsage(first, next)
	if err != nil || !merged.CounterReset || merged.Samples != 2 || merged.MemoryPeakBytes == nil || *merged.MemoryPeakBytes != 200000 || merged.CPUPeakNanoCores == nil || *merged.CPUPeakNanoCores != 1000000000 {
		t.Fatalf("fresh cAdvisor sample dropped behind summary clock: %#v %v", merged, err)
	}
	again, err := model.MergeResourceUsage(merged, next)
	if err != nil || again != merged {
		t.Fatal("cached duplicate caused a telemetry write")
	}
}

func TestResourceMemoryHighWaterCatchesBetweenPollPeakAndFallsBack(t *testing.T) {
	b, p, s, at := sampleFixture(t)
	high := s.Metrics[0]
	high.Name = "container_memory_max_usage_bytes"
	high.Value = 900000
	raw := fmt.Sprintf("%s{namespace=%q,pod=%q,container=\"main\",id=%q} %g %d\n", high.Name, p.Namespace, p.Name, high.Labels["id"], high.Value, at.UnixMilli())
	metrics, err := ParseMetrics([]byte(raw))
	if err != nil || len(metrics) != 1 {
		t.Fatalf("high-water parser: %v %#v", err, metrics)
	}
	s.Metrics = append(s.Metrics, metrics...)
	observed, ok := Observe(b, p, s, at)
	if !ok || observed.MemoryPeakBytes == nil || *observed.MemoryPeakBytes != 900000 {
		t.Fatalf("high-water peak lost: %#v", observed)
	}
	for _, kind := range []string{"stale", "foreign-id", "zero", "absent"} {
		t.Run(kind, func(t *testing.T) {
			b, p, s, at := sampleFixture(t)
			h := s.Metrics[0]
			h.Name = "container_memory_max_usage_bytes"
			h.Value = 900000
			// Clone labels before making the high-water metric foreign.
			h.Labels = map[string]string{}
			for k, v := range s.Metrics[0].Labels {
				h.Labels[k] = v
			}
			switch kind {
			case "stale":
				h.At = at.Add(-time.Minute)
			case "foreign-id":
				h.Labels["id"] += "foreign"
			case "zero":
				h.Value = 0
			}
			if kind != "absent" {
				s.Metrics = append(s.Metrics, h)
			}
			observed, ok := Observe(b, p, s, at)
			if !ok || observed.MemoryPeakBytes == nil || *observed.MemoryPeakBytes != 123456 {
				t.Fatalf("gauge fallback %s: %#v", kind, observed)
			}
		})
	}
}

func TestResourceDefaultCadenceMatchesHousekeeping(t *testing.T) {
	if DefaultInterval != 15*time.Second || (Config{}).PollInterval() != 15*time.Second {
		t.Fatal("default must be 15s to avoid repeated cAdvisor samples")
	}
}
func TestResourcePodScopeRequestsPSIAndSaturation(t *testing.T) {
	b, p, s, at := sampleFixture(t)
	p.Spec.InitContainers = []corev1.Container{{Name: "sidecar", RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways), Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("2Gi")}}}, {Name: "init", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("4Gi")}}}}
	if ephemeralRequest(p) != 6<<30 {
		t.Fatal("restartable init overlap missing")
	}
	var summary Summary
	summary.Node.NodeName = p.Spec.NodeName

	// Exercise the actual kubelet summary decoder and nanosecond PSI units.
	raw := fmt.Sprintf(`{"node":{"nodeName":"node-a"},"pods":[{"podRef":{"name":%q,"namespace":%q,"uid":%q},"ephemeral-storage":{"time":%q,"usedBytes":42},"io":{"time":%q,"psi":{"some":{"total":123000000},"full":{"total":0}}}}]}`, p.Name, p.Namespace, p.UID, at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
	if err := json.Unmarshal([]byte(raw), &summary); err != nil {
		t.Fatal(err)
	}
	s.Summary = summary
	s.SummaryOK = true
	v, ok := Observe(b, p, s, at)
	if !ok || *v.EphemeralStoragePeakBytes != 42 || *v.IOPressureSomeNS != 123000000 || *v.IOPressureFullNS != 0 || *v.EphemeralRequestBytes != 6<<30 {
		t.Fatalf("pod scope %#v", v)
	}
	sat := false
	if *unsigned(math.MaxUint64, &sat) != math.MaxInt64 || !sat {
		t.Fatal("overflow")
	}
}
