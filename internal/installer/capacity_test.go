package installer

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestFrozenCapacityAdviceUsesNativeFanoutAndBaseline(t *testing.T) {
	for _, tc := range []struct {
		cpu, memory string
		pods, want  int
	}{
		{"2100m", "6Gi", 1, 4}, {"1100m", "4Gi", 1, 8}, {"2100m", "6Gi", 2, 2},
	} {
		e := capacityFixture(tc.cpu, tc.memory, tc.pods)
		before, _ := json.Marshal(e)
		a := AdviseCapacity(e)
		if a.Parallel == nil || *a.Parallel != tc.want {
			t.Fatalf("want %d, got %+v", tc.want, a)
		}
		if !reflect.DeepEqual(a, AdviseCapacity(e)) {
			t.Fatal("fixed snapshot changed advice")
		}
		after, _ := json.Marshal(e)
		if string(before) != string(after) {
			t.Fatal("advice changed frozen evidence")
		}
	}
	e := capacityFixture("2100m", "6Gi", 1)
	e.NonRunAndMaintenance[0].Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("4500m")
	if a := AdviseCapacity(e); a.Parallel == nil || *a.Parallel != 2 {
		t.Fatal("changed baseline did not change advice", a)
	}
}

func TestCapacityAdviceRefusesEvidenceGapsAndUnsafeWorstCase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*CapacityEvidence)
	}{
		{"missing G1", func(e *CapacityEvidence) { delete(e.Receipts, "G1") }},
		{"missing G2", func(e *CapacityEvidence) { delete(e.Receipts, "G2") }},
		{"missing G3", func(e *CapacityEvidence) { delete(e.Receipts, "G3") }},
		{"missing G7", func(e *CapacityEvidence) { delete(e.Receipts, "G7") }},
		{"unknown children", func(e *CapacityEvidence) { e.Families[0].UnknownShapes = true }},
		{"too few executions", func(e *CapacityEvidence) { e.Families[0].Executions = 19 }},
		{"missing joins", func(e *CapacityEvidence) { e.Families[0].JoinedPods = 98 }},
		{"zero-Pod waiter", func(e *CapacityEvidence) { e.Families[0].Intervals[0].Pods = nil }},
		{"missing helper request", func(e *CapacityEvidence) {
			delete(e.Families[0].Intervals[0].Pods[0].Spec.Containers[0].Resources.Requests, corev1.ResourceCPU)
		}},
		{"missing limit", func(e *CapacityEvidence) {
			delete(e.Families[0].WorstPods[0].Spec.Containers[0].Resources.Limits, corev1.ResourceMemory)
		}},
		{"unknown namespace", func(e *CapacityEvidence) { e.Families[0].WorstPods[0].Namespace = "outside" }},
		{"duplicate quota", func(e *CapacityEvidence) { e.Quotas = append(e.Quotas, e.Quotas[0]) }},
		{"no feasible node", func(e *CapacityEvidence) { e.Node.Spec.Unschedulable = true }},
		{"selector", func(e *CapacityEvidence) {
			e.Families[0].WorstPods[0].Spec.NodeSelector = map[string]string{"absent": "yes"}
		}},
		{"taint", func(e *CapacityEvidence) {
			e.Node.Spec.Taints = []corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectNoSchedule}}
		}},
		{"zero cap", func(e *CapacityEvidence) { e.CurrentCap = 0 }},
		{"worst shape", func(e *CapacityEvidence) {
			e.Families[0].WorstPods = append(e.Families[0].WorstPods, capacityPod("extra", "3", "6Gi"))
		}},
		{"pressure", func(e *CapacityEvidence) {
			e.Node.Status.Conditions = append(e.Node.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := capacityFixture("2100m", "6Gi", 1)
			tc.change(&e)
			a := AdviseCapacity(e)
			if a.Parallel != nil || a.Reason != capacityReason {
				t.Fatal("unsafe recommendation", a)
			}
		})
	}
}

func TestCapacityNativeRestartableInitAndOverhead(t *testing.T) {
	pod := capacityPod("sidecar", "1", "1Gi")
	restart := corev1.ContainerRestartPolicyAlways
	sidecar := pod.Spec.Containers[0].DeepCopy()
	sidecar.Name = "sidecar"
	sidecar.RestartPolicy = &restart
	init := pod.Spec.Containers[0].DeepCopy()
	init.Name = "init"
	init.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("2")
	pod.Spec.InitContainers = []corev1.Container{*sidecar, *init}
	pod.Spec.Overhead = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}
	vector, ok := podVector(pod, true)
	if !ok || vector[0] != 3100 || vector[1] != (2<<30)+(128<<20) || vector[3] != 1 {
		t.Fatal("native init overlap/overhead lost", vector, ok)
	}
}

func TestCapacityMixWeightsFamiliesAndActiveIntervals(t *testing.T) {
	samples := []capacitySample{{vector: [6]int64{1}, weight: 95}, {vector: [6]int64{10}, weight: 5}}
	if got := capacityPercentile(samples, 0); got != 1 {
		t.Fatal("nearest-rank boundary", got)
	}
	if len(capacityReason) > 80 || len(capacityRemedy) > 120 {
		t.Fatal("capacity message exceeds bounds")
	}
}

func TestCapacityFamilyMixDoesNotWeightBySampleCount(t *testing.T) {
	e := capacityFixture("1100m", "4Gi", 1)
	heavy := capacityFixture("2100m", "6Gi", 1).Families[0]
	e.Families[0].Name = "ordinary"
	e.Families[0].MixWeight = 90
	heavy.Name = "heavy"
	heavy.MixWeight = 10
	for range 9 {
		heavy.Intervals = append(heavy.Intervals, heavy.Intervals[0])
	}
	e.Families = append(e.Families, heavy)
	samples, _, ok := capacitySamples(e)
	if !ok || capacityPercentile(samples, 0) != 2100 {
		t.Fatal("frozen mix lost high family", samples, ok)
	}
	e.Families[0].MixWeight = 96
	e.Families[1].MixWeight = 4
	samples, _, ok = capacitySamples(e)
	if !ok || capacityPercentile(samples, 0) != 1100 {
		t.Fatal("sampling frequency replaced family mix", samples, ok)
	}
}

func TestCapacityAdviceRequiresEachNamespaceShareToFit(t *testing.T) {
	e := capacityFixture("2100m", "6Gi", 1)
	e.Quotas = append(e.Quotas, *e.Quotas[0].DeepCopy())
	e.Quotas[1].Namespace = "children"
	for i := range e.Quotas {
		e.Quotas[i].Spec.Hard[corev1.ResourceRequestsCPU] = resource.MustParse("4500m")
	}
	if a := AdviseCapacity(e); a.Parallel != nil {
		t.Fatal("aggregate fit bypassed a namespace share", a)
	}
}

func TestCapacityAdviceDeclinesUnmodelledPlacement(t *testing.T) {
	for _, modify := range []func(*corev1.Pod){
		func(p *corev1.Pod) { p.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}} },
		func(p *corev1.Pod) {
			p.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{TopologyKey: "kubernetes.io/hostname", MaxSkew: 1, WhenUnsatisfiable: corev1.DoNotSchedule}}
		},
		func(p *corev1.Pod) {
			p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: "example.invalid/hold"}}
		},
	} {
		e := capacityFixture("2100m", "6Gi", 1)
		modify(&e.Families[0].WorstPods[0])
		if a := AdviseCapacity(e); a.Parallel != nil {
			t.Fatal("unmodelled placement produced advice", a)
		}
	}
}

func capacityFixture(cpu, memory string, pods int) CapacityEvidence {
	e := CapacityEvidence{Snapshot: "fixture", CapturedAt: time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC), CurrentCap: 8, Origin: "chosen", OperatingScratch: resource.MustParse("256Gi"), Receipts: map[string]string{"G1": "g1.json", "G2": "g2.json", "G3": "g3.json", "G7": "g7.json", "placement": "placement.json"}}
	e.Node = corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10500m"), corev1.ResourceMemory: resource.MustParse("56Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("844Gi"), corev1.ResourcePods: resource.MustParse("110")}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	for _, kind := range []corev1.NodeConditionType{corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure} {
		e.Node.Status.Conditions = append(e.Node.Status.Conditions, corev1.NodeCondition{Type: kind, Status: corev1.ConditionFalse})
	}
	baseline := capacityPod("baseline", "1500m", "8Gi")
	baseline.Namespace = "control"
	e.NonRunAndMaintenance = []corev1.Pod{baseline}
	e.Quotas = []corev1.ResourceQuota{{ObjectMeta: metav1.ObjectMeta{Namespace: "runs"}, Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("9"), corev1.ResourceRequestsMemory: resource.MustParse("48Gi"), corev1.ResourceLimitsMemory: resource.MustParse("48Gi"), corev1.ResourcePods: resource.MustParse("80"), corev1.ResourceRequestsEphemeralStorage: resource.MustParse("256Gi"), corev1.ResourceLimitsEphemeralStorage: resource.MustParse("256Gi")}}}}
	family := CapacityFamily{Name: "family", MixWeight: 1, Executions: 20, JoinedPods: 100, SampledPods: 100}
	for i := range pods {
		family.WorstPods = append(family.WorstPods, capacityPod(fmt.Sprint(i), cpu, memory))
	}
	family.Intervals = []CapacityInterval{{Seconds: 20, Pods: deepCopyPods(family.WorstPods)}}
	e.Families = []CapacityFamily{family}
	return e
}

func capacityPod(id, cpu, memory string) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: id, UID: types.UID(id), Namespace: "runs"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory), corev1.ResourceEphemeralStorage: resource.MustParse("16Gi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory), corev1.ResourceEphemeralStorage: resource.MustParse("16Gi")}}}}}}
}

func deepCopyPods(pods []corev1.Pod) []corev1.Pod {
	var result []corev1.Pod
	for _, pod := range pods {
		result = append(result, *pod.DeepCopy())
	}
	return result
}
