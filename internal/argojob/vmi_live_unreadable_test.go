package argojob

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestVMICreateUnreadablePresentCountCannotBorrowClaim(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
		missing     bool
	}{
		{name: "string-cores", field: "cores", value: "64"},
		{name: "fractional-cores", field: "cores", value: 15.5},
		{name: "zero-cores", field: "cores", value: int64(0)},
		{name: "negative-sockets", field: "sockets", value: int64(-1)},
		{name: "boolean-threads", field: "threads", value: true},
		{name: "null-cores", field: "cores", value: nil},
		{name: "malformed-parent", field: "parent", value: "64"},
		{name: "missing-cores-control", field: "cores", missing: true},
		{name: "missing-sockets-control", field: "sockets", missing: true},
		{name: "missing-parent-control", field: "parent", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			object.Object["spec"] = liveVMIFixture(t, "hotplug-defaulted").Object["spec"]
			path := []string{"spec", "domain", "cpu"}
			if tc.field != "parent" {
				path = append(path, tc.field)
			}
			if tc.missing {
				unstructured.RemoveNestedField(object.Object, path...)
			} else if err := unstructured.SetNestedField(object.Object, tc.value, path...); err != nil {
				t.Fatal(err)
			}
			if _, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Update(t.Context(), object, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			// Restart: even an exact, durable two-core claim cannot bound an
			// invalid present count. Only genuinely absent fields can borrow it.
			c = quotaController(config, kube, vmi)
			_, second, sp := guardedVMISourceFixture(t, 2, 7, source)
			err = authorizeTestVMI(t, c, second, sp)
			if tc.missing {
				if err != nil {
					t.Fatalf("missing-field claim fallback changed: %v", err)
				}
			} else {
				full := argoworkflow.DefaultVMIBudget().DemandLimit()
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("repository used=%+v", full)) {
					t.Fatalf("unreadable value borrowed a smaller claim instead of full grant: %v", err)
				}
				// Repair the malformed live object: its original exact claim
				// must remain intact, and two ordinary 6Gi VMIs fit again.
				object.Object["spec"] = liveVMIFixture(t, "hotplug-defaulted").Object["spec"]
				if _, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Update(t.Context(), object, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				if err := authorizeTestVMI(t, c, second, sp); err != nil {
					t.Fatalf("repaired live VMI retained a capacity leak: %v", err)
				}
			}
			_, other, op := guardedVMISourceFixture(t, 3, 8, source)
			if err := authorizeTestVMI(t, c, other, op); err != nil {
				t.Fatalf("another repository was blocked: %v", err)
			}
		})
	}
}

func TestVMICreateOversizeUnlabeledObjectBlocksEveryRepository(t *testing.T) {
	kube, vmi := casQuotaClient(), quotaDynamic()
	source := strings.ReplaceAll(kubeVirtWorkflow, "2Gi", "6Gi")
	config, wf, _ := guardedVMISourceFixture(t, 1, 7, source)
	object := liveVMIFixture(t, "hotplug-defaulted")
	object.SetName("oversized-unlabeled")
	object.SetNamespace(wf.Namespace)
	object.SetLabels(nil)
	object.SetOwnerReferences(nil)
	setLiveVMIField(t, object, "memory.guest", "300Gi")
	if _, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Create(t.Context(), object, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c := quotaController(config, kube, vmi)
	for _, repo := range []int64{7, 8} {
		_, run, pod := guardedVMISourceFixture(t, int(repo), repo, source)
		if err := authorizeTestVMI(t, c, run, pod); err == nil || !strings.Contains(err.Error(), "over grant") {
			t.Fatalf("unattributed oversize VMI was not charged to repo %d: %v", repo, err)
		}
	}
	if err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Delete(t.Context(), object.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []int64{7, 8} {
		_, run, pod := guardedVMISourceFixture(t, int(repo), repo, source)
		if err := authorizeTestVMI(t, c, run, pod); err != nil {
			t.Fatalf("removed unattributed VMI still blocked repo %d: %v", repo, err)
		}
	}
}
