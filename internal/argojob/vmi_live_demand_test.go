package argojob

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func liveVMIFixture(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	body, err := os.ReadFile("testdata/vmi-live/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	object := &unstructured.Unstructured{}
	if err = json.Unmarshal(body, &object.Object); err != nil {
		t.Fatal(err)
	}
	return object
}

func TestLiveVMIDemandDefaultedJSON(t *testing.T) {
	for _, name := range []string{"hotplug-defaulted", "guest-and-requests", "requests-only"} {
		t.Run(name, func(t *testing.T) {
			demand, err := liveVMIDemand(liveVMIFixture(t, name))
			want := argoworkflow.VMIDemand{VMs: 1, Cores: 2, MemoryBytes: 6 << 30}
			if err != nil || demand != want {
				t.Fatalf("6Gi guest must remain 6Gi, not 24Gi: got %+v, %v", demand, err)
			}
		})
	}
}

func TestLiveVMIDemandCurrentFields(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
		cores       int
		memory      int64
		bad         bool
	}{
		{name: "current-sockets", field: "cpu.sockets", value: int64(2), cores: 4},
		{name: "current-threads", field: "cpu.threads", value: int64(2), cores: 4},
		{name: "current-guest", field: "memory.guest", value: "8Gi", memory: 8 << 30},
		{name: "ignore-large-hotplug", field: "memory.maxGuest", value: "1024Gi"},
		{name: "ignore-large-maxSockets", field: "cpu.maxSockets", value: int64(1024)},
		{name: "missing-cores", field: "cpu.cores", bad: true},
		{name: "missing-sockets-default-one", field: "cpu.sockets"},
		{name: "missing-threads-default-one", field: "cpu.threads"},
		{name: "zero-sockets", field: "cpu.sockets", value: int64(0), bad: true},
		{name: "negative-threads", field: "cpu.threads", value: int64(-1), bad: true},
		{name: "huge-cores", field: "cpu.cores", value: int64(1 << 62), bad: true},
		{name: "topology-over-bound", field: "cpu.sockets", value: int64(64), bad: true},
		{name: "invalid-guest-not-hidden-by-request", field: "memory.guest", value: "invalid", bad: true},
		{name: "negative-guest", field: "memory.guest", value: "-6Gi", bad: true},
		{name: "wrong-guest-type", field: "memory.guest", value: true, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := liveVMIFixture(t, "hotplug-defaulted")
			path := append([]string{"spec", "domain"}, strings.Split(tc.field, ".")...)
			if tc.value == nil {
				unstructured.RemoveNestedField(object.Object, path...)
			} else if err := unstructured.SetNestedField(object.Object, tc.value, path...); err != nil {
				t.Fatal(err)
			}
			demand, err := liveVMIDemand(object)
			if tc.bad {
				if err == nil {
					t.Fatalf("ambiguous demand accepted: %+v", demand)
				}
				return
			}
			want := argoworkflow.VMIDemand{VMs: 1, Cores: max(2, tc.cores), MemoryBytes: max(int64(6<<30), tc.memory)}
			if err != nil || demand != want {
				t.Fatalf("got %+v, %v; want %+v", demand, err, want)
			}
		})
	}
	object := liveVMIFixture(t, "hotplug-defaulted")
	unstructured.RemoveNestedField(object.Object, "spec", "domain", "memory")
	unstructured.RemoveNestedField(object.Object, "spec", "domain", "resources")
	for _, invalid := range []*unstructured.Unstructured{object, nil, {Object: map[string]any{"kind": "Pod"}}} {
		if _, err := liveVMIDemand(invalid); err == nil {
			t.Fatal("missing memory or invalid VMI accepted")
		}
	}
}

func TestVMICreateDefaultedLiveDemandAndClaimFallback(t *testing.T) {
	for _, mode := range []string{"hotplug-defaulted", "guest-and-requests", "requests-only", "missing-cpu", "invalid-memory", "missing-memory", "over-current-memory", "over-accounting-memory", "over-accounting-cpu", "over-memory-without-cpu", "over-current-memory-without-cpu", "over-current-cpu-without-memory"} {
		t.Run(mode, func(t *testing.T) {
			kube, vmi := casQuotaClient(), quotaDynamic()
			source := strings.ReplaceAll(kubeVirtWorkflow, "2Gi", "6Gi")
			config, wf, pod := guardedVMISourceFixture(t, 1, 7, source)
			c := quotaController(config, kube, vmi)
			if err := authorizeTestVMI(t, c, wf, pod); err != nil {
				t.Fatal(err)
			}
			object, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Get(t.Context(), wf.Name+"-amd64", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			fixture := "hotplug-defaulted"
			if mode == "guest-and-requests" || mode == "requests-only" {
				fixture = mode
			}
			object.Object["spec"] = liveVMIFixture(t, fixture).Object["spec"]
			switch mode {
			case "missing-cpu":
				unstructured.RemoveNestedField(object.Object, "spec", "domain", "cpu")
			case "invalid-memory":
				if err = unstructured.SetNestedField(object.Object, "invalid", "spec", "domain", "memory", "guest"); err != nil {
					t.Fatal(err)
				}
			case "over-current-memory", "over-accounting-memory", "over-memory-without-cpu", "over-current-memory-without-cpu":
				value := "8Gi"
				if mode != "over-current-memory" && mode != "over-current-memory-without-cpu" {
					value = "512Gi"
				}
				if err = unstructured.SetNestedField(object.Object, value, "spec", "domain", "memory", "guest"); err != nil {
					t.Fatal(err)
				}
				if mode == "over-memory-without-cpu" || mode == "over-current-memory-without-cpu" {
					unstructured.RemoveNestedField(object.Object, "spec", "domain", "cpu")
				}
			case "over-current-cpu-without-memory":
				unstructured.RemoveNestedField(object.Object, "spec", "domain", "memory")
				unstructured.RemoveNestedField(object.Object, "spec", "domain", "resources")
				if err = unstructured.SetNestedField(object.Object, int64(3), "spec", "domain", "cpu", "sockets"); err != nil {
					t.Fatal(err)
				}
			case "over-accounting-cpu":
				if err = unstructured.SetNestedField(object.Object, int64(128), "spec", "domain", "cpu", "sockets"); err != nil {
					t.Fatal(err)
				}
			case "missing-memory":
				unstructured.RemoveNestedField(object.Object, "spec", "domain", "memory")
				unstructured.RemoveNestedField(object.Object, "spec", "domain", "resources")
			}
			if _, err = vmi.Resource(vmiResource).Namespace(wf.Namespace).Update(t.Context(), object, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			// A restart must use persistent template demand for unreadable fields.
			c = quotaController(config, kube, vmi)
			_, second, sp := guardedVMISourceFixture(t, 2, 7, source)
			err = authorizeTestVMI(t, c, second, sp)
			if strings.HasPrefix(mode, "over-") {
				if err == nil || !strings.Contains(err.Error(), "over grant") {
					t.Fatalf("oversized current demand must not borrow a smaller claim: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("two 6Gi guests must fit the 12Gi grant: %v", err)
			}
			_, third, tp := guardedVMISourceFixture(t, 3, 7, source)
			if err = authorizeTestVMI(t, c, third, tp); err == nil || !strings.Contains(err.Error(), "over grant") {
				t.Fatalf("third guest must still be refused: %v", err)
			}
		})
	}
}

func TestLiveVMIClaimFallbackRequiresExactIdentity(t *testing.T) {
	kube, vmi := casQuotaClient(), quotaDynamic()
	config, wf, pod := guardedVMIFixture(t, 1)
	c := quotaController(config, kube, vmi)
	if err := authorizeTestVMI(t, c, wf, pod); err != nil {
		t.Fatal(err)
	}
	cm, err := c.getVMILedger(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := readVMILedger(cm)
	if err != nil {
		t.Fatal(err)
	}
	object, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Get(t.Context(), wf.Name+"-amd64", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	limit := argoworkflow.DefaultVMIBudget().DemandLimit()
	declared := argoworkflow.VMIDemand{VMs: 1, Cores: 2, MemoryBytes: 2 << 30}
	if got := liveVMIClaimFallback(object, ledger, wf.Namespace, limit); got != declared {
		t.Fatalf("fallback %+v != declared %+v", got, declared)
	}
	for _, change := range []string{"uid", "hash", "operation", "owner", "repository", "namespace", "name", "closed", "pending"} {
		t.Run(change, func(t *testing.T) {
			copy := object.DeepCopy()
			claims := vmiLedger{Version: ledger.Version, Claims: map[string]vmiClaim{}}
			for key, claim := range ledger.Claims {
				switch change {
				case "closed":
					claim.Closed = true
				case "pending":
					claim.UID = ""
				}
				claims.Claims[key] = claim
			}
			switch change {
			case "uid":
				copy.SetUID(types.UID("replacement"))
			case "hash", "operation":
				annotations := copy.GetAnnotations()
				key := vmiCreateHashAnnotation
				if change == "operation" {
					key = vmiOperationAnnotation
				}
				annotations[key] = "different"
				copy.SetAnnotations(annotations)
			case "owner":
				copy.SetOwnerReferences(nil)
			case "repository":
				copy.SetLabels(map[string]string{vmiRepositoryLabel: "8"})
			case "namespace":
				copy.SetNamespace("different")
			case "name":
				copy.SetName("different")
			}
			if got := liveVMIClaimFallback(copy, claims, wf.Namespace, limit); got != limit {
				t.Fatalf("identity mismatch borrowed smaller claim: %+v", got)
			}
		})
	}
	shrunk := argoworkflow.VMIDemand{VMs: 1, Cores: 1, MemoryBytes: 1 << 30}
	if got := liveVMIClaimFallback(object, ledger, wf.Namespace, shrunk); got != shrunk {
		t.Fatalf("fallback alone exceeds new repository limit: %+v", got)
	}
	object.SetLabels(nil)
	if got := liveVMIClaimFallback(object, ledger, wf.Namespace, limit); got != argoworkflow.VMIInstanceLimit() {
		t.Fatalf("unattributed object lost legacy maximum fallback: %+v", got)
	}
}
