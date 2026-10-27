package argojob

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func setLiveVMIField(t *testing.T, object *unstructured.Unstructured, field string, value any) {
	t.Helper()
	path := append([]string{"spec", "domain"}, strings.Split(field, ".")...)
	if value == nil {
		unstructured.RemoveNestedField(object.Object, path...)
	} else if number, ok := value.(json.Number); ok {
		parent, found, err := unstructured.NestedFieldNoCopy(object.Object, path[:len(path)-1]...)
		if err != nil || !found {
			t.Fatalf("missing numeric field parent: %v", err)
		}
		parent.(map[string]any)[path[len(path)-1]] = number
	} else if err := unstructured.SetNestedField(object.Object, value, path...); err != nil {
		t.Fatal(err)
	}
}

func TestLiveVMIDemandRetainsReadableLowerBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edits  map[string]any
		cores  int
		memory int64
		bad    bool
	}{
		{name: "requests-memory-floor", edits: map[string]any{"memory.guest": "1Gi", "resources.requests.memory": "200Gi"}, cores: 2, memory: 200 << 30},
		{name: "missing-cores-with-sockets", edits: map[string]any{"cpu.cores": nil, "cpu.sockets": int64(16)}, cores: 16, memory: 6 << 30, bad: true},
		{name: "zero-cores-with-sockets-and-threads", edits: map[string]any{"cpu.cores": int64(0), "cpu.sockets": int64(16), "cpu.threads": int64(2)}, cores: 32, memory: 6 << 30, bad: true},
		{name: "negative-cores-with-sockets", edits: map[string]any{"cpu.cores": int64(-1), "cpu.sockets": int64(16)}, cores: 16, memory: 6 << 30, bad: true},
		{name: "string-cores-readable-memory", edits: map[string]any{"cpu.cores": "2", "memory.guest": "200Gi"}, cores: 1, memory: 200 << 30, bad: true},
		{name: "overflowing-cores-readable-memory", edits: map[string]any{"cpu.cores": float64(1e20), "memory.guest": "200Gi"}, cores: 65, memory: 200 << 30, bad: true},
		{name: "wrong-cpu-parent-readable-memory", edits: map[string]any{"cpu": "invalid", "memory.guest": "200Gi"}, cores: 1, memory: 200 << 30, bad: true},
		{name: "wrong-guest-type-readable-cpu-and-request", edits: map[string]any{"memory.guest": true, "cpu.sockets": int64(8)}, cores: 16, memory: 6 << 30, bad: true},
		{name: "wrong-request-type-readable-guest", edits: map[string]any{"resources.requests.memory": true, "memory.guest": "200Gi"}, cores: 2, memory: 200 << 30, bad: true},
		{name: "fractional-count-retains-sockets", edits: map[string]any{"cpu.cores": float64(2.5), "cpu.sockets": int64(16)}, cores: 16, memory: 6 << 30, bad: true},
		{name: "json-number-counts", edits: map[string]any{"cpu.cores": json.Number("2"), "cpu.sockets": json.Number("2")}, cores: 4, memory: 6 << 30},
		{name: "exponent-count-retains-memory", edits: map[string]any{"cpu.cores": json.Number("1e20"), "memory.guest": "200Gi"}, cores: 65, memory: 200 << 30, bad: true},
		{name: "huge-exponent-count-retains-memory", edits: map[string]any{"cpu.cores": json.Number("1e1000"), "memory.guest": "200Gi"}, cores: 65, memory: 200 << 30, bad: true},
		{name: "numeric-request-quantity", edits: map[string]any{"resources.requests.memory": int64(6 << 30)}, cores: 2, memory: 6 << 30},
		{name: "wrong-memory-parent-readable-cpu-and-request", edits: map[string]any{"memory": "invalid", "cpu.sockets": int64(8)}, cores: 16, memory: 6 << 30, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := liveVMIFixture(t, "hotplug-defaulted")
			for field, value := range tc.edits {
				setLiveVMIField(t, object, field, value)
			}
			demand, err := liveVMIDemand(object)
			if (err != nil) != tc.bad || demand.VMs != 1 || demand.Cores < tc.cores || demand.MemoryBytes < tc.memory {
				t.Fatalf("readable lower bound lost: got %+v, err=%v; want >= 1 VM/%d cores/%d bytes, error=%v", demand, err, tc.cores, tc.memory, tc.bad)
			}
		})
	}
}

func TestVMICreateClaimFloorAndMalformedLiveDimensions(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		firstCPU, secondCPU       int
		firstMemory, secondMemory string
		edits                     map[string]any
		allow                     bool
	}{
		{name: "claim-memory-floor", firstCPU: 2, secondCPU: 2, firstMemory: "6Gi", secondMemory: "8Gi", edits: map[string]any{"memory.guest": "1Gi", "resources.requests.memory": "1Gi"}},
		{name: "claim-cpu-floor", firstCPU: 4, secondCPU: 4, firstMemory: "6Gi", secondMemory: "6Gi", edits: map[string]any{"cpu.cores": int64(1)}},
		{name: "requests-memory-floor", firstCPU: 2, secondCPU: 2, firstMemory: "6Gi", secondMemory: "6Gi", edits: map[string]any{"memory.guest": "1Gi", "resources.requests.memory": "200Gi"}},
		{name: "missing-cores-retains-sockets", firstCPU: 2, secondCPU: 2, firstMemory: "6Gi", secondMemory: "6Gi", edits: map[string]any{"cpu.cores": nil, "cpu.sockets": int64(16)}},
		{name: "zero-cores-retains-sockets", firstCPU: 2, secondCPU: 2, firstMemory: "6Gi", secondMemory: "6Gi", edits: map[string]any{"cpu.cores": int64(0), "cpu.sockets": int64(16)}},
		{name: "cpu-type-error-retains-memory", firstCPU: 2, secondCPU: 2, firstMemory: "6Gi", secondMemory: "6Gi", edits: map[string]any{"cpu.cores": "2", "memory.guest": "200Gi"}},
		{name: "cpu-overflow-retains-memory", firstCPU: 2, secondCPU: 2, firstMemory: "6Gi", secondMemory: "6Gi", edits: map[string]any{"cpu.cores": float64(1e20)}},
		{name: "memory-type-error-retains-cpu", firstCPU: 2, secondCPU: 2, firstMemory: "6Gi", secondMemory: "6Gi", edits: map[string]any{"memory.guest": true, "cpu.sockets": int64(8)}},
		{name: "claim-floor-is-max-not-sum", firstCPU: 2, secondCPU: 2, firstMemory: "6Gi", secondMemory: "6Gi", edits: map[string]any{"memory.guest": "1Gi", "resources.requests.memory": "1Gi", "cpu.cores": int64(1)}, allow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube, vmi := casQuotaClient(), quotaDynamic()
			source := func(cpu int, memory string) string {
				text := strings.ReplaceAll(kubeVirtWorkflow, "cores: 2", fmt.Sprintf("cores: %d", cpu))
				return strings.ReplaceAll(text, "memory: 2Gi", "memory: "+memory)
			}
			config, wf, pod := guardedVMISourceFixture(t, 1, 7, source(tc.firstCPU, tc.firstMemory))
			c := quotaController(config, kube, vmi)
			if err := authorizeTestVMI(t, c, wf, pod); err != nil {
				t.Fatal(err)
			}
			object, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Get(t.Context(), wf.Name+"-amd64", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			object.Object["spec"] = liveVMIFixture(t, "hotplug-defaulted").Object["spec"]
			for field, value := range tc.edits {
				setLiveVMIField(t, object, field, value)
			}
			if _, err = vmi.Resource(vmiResource).Namespace(wf.Namespace).Update(t.Context(), object, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			// Use the persisted claim after a controller restart, not local state.
			c = quotaController(config, kube, vmi)
			_, next, np := guardedVMISourceFixture(t, 2, 7, source(tc.secondCPU, tc.secondMemory))
			err = authorizeTestVMI(t, c, next, np)
			if tc.allow {
				if err != nil {
					t.Fatalf("max(live, claim) must not double-charge the same VMI: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "over grant") {
				t.Fatalf("live/claim lower bound did not refuse the additional VMI: %v", err)
			}
			_, other, op := guardedVMISourceFixture(t, 3, 8, source(2, "6Gi"))
			if err = authorizeTestVMI(t, c, other, op); err != nil {
				t.Fatalf("repo 7's malformed or large VMI blocked repo 8: %v", err)
			}
		})
	}
}

func TestLiveVMIDemandOversizedDimensionsSaturate(t *testing.T) {
	object := liveVMIFixture(t, "hotplug-defaulted")
	setLiveVMIField(t, object, "cpu.cores", int64(math.MaxInt64))
	setLiveVMIField(t, object, "cpu.sockets", int64(16))
	setLiveVMIField(t, object, "cpu.threads", int64(16))
	setLiveVMIField(t, object, "memory.guest", "1e40")
	demand, err := liveVMIDemand(object)
	if !errors.Is(err, errLiveVMIOverLimit) || demand.Cores != math.MaxInt || demand.MemoryBytes != math.MaxInt64 {
		t.Fatalf("oversized dimensions wrapped or disappeared: %+v %v", demand, err)
	}
	combined := addVMIDemand(demand, demand)
	if combined.Cores < demand.Cores || combined.MemoryBytes < demand.MemoryBytes {
		t.Fatalf("aggregation overflowed: %+v -> %+v", demand, combined)
	}
}
