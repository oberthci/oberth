package argojob

import (
	"encoding/json"
	"errors"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

var errLiveVMIOverLimit = errors.New("argojob: live VMI current demand exceeds the accounting bound")

// Live VMIs can include KubeVirt defaults and legacy/admin fields which are
// deliberately forbidden in authored manifests. Count current guest resources,
// not hotplug ceilings or launcher limits. Unreadable demand needs a trusted
// claim fallback at the caller; it must never become free capacity. On an
// ambiguous field, the error result retains other readable current dimensions
// so a smaller claim cannot hide a known increase.
func liveVMIDemand(object *unstructured.Unstructured) (argoworkflow.VMIDemand, error) {
	fail := errors.New("argojob: cannot bound live VMI demand")
	var live struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Spec       struct {
			Domain struct {
				CPU       struct{ Cores, Sockets, Threads *int }
				Memory    struct{ Guest string }
				Resources struct{ Requests struct{ Memory string } }
			}
		}
	}
	if object == nil {
		return argoworkflow.VMIDemand{}, fail
	}
	raw, err := json.Marshal(object.Object)
	if err != nil {
		return argoworkflow.VMIDemand{}, err
	}
	if err = json.Unmarshal(raw, &live); err != nil || live.APIVersion != "kubevirt.io/v1" || live.Kind != "VirtualMachineInstance" {
		return argoworkflow.VMIDemand{}, fail
	}
	limit := argoworkflow.VMIBudgetLimit()
	d := live.Spec.Domain
	cores := 1
	// KubeVirt defaults omitted sockets/threads to one. Missing cores is
	// ambiguous (legacy request-only CPU shapes), so use the claim instead.
	ambiguous := d.CPU.Cores == nil
	for _, count := range []*int{d.CPU.Cores, d.CPU.Sockets, d.CPU.Threads} {
		if count == nil {
			continue
		}
		if *count <= 0 {
			ambiguous = true
			continue
		}
		if *count > limit.MaxCores || cores > limit.MaxCores / *count {
			return argoworkflow.VMIDemand{}, errLiveVMIOverLimit
		}
		cores *= *count
	}
	value := d.Memory.Guest
	if value == "" {
		value = d.Resources.Requests.Memory
	}
	memory, err := resource.ParseQuantity(value)
	if err == nil && memory.Cmp(limit.MaxMemory) > 0 {
		return argoworkflow.VMIDemand{}, errLiveVMIOverLimit
	}
	demand := argoworkflow.VMIDemand{VMs: 1}
	if !ambiguous {
		demand.Cores = cores
	}
	if err == nil && memory.Sign() > 0 {
		demand.MemoryBytes = memory.Value()
	}
	if ambiguous || err != nil || memory.Sign() <= 0 {
		return demand, fail
	}
	if !validVMIDemand(demand) {
		return argoworkflow.VMIDemand{}, fail
	}
	return demand, nil
}
