package argojob

import (
	"encoding/json"
	"errors"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Live VMIs can include KubeVirt defaults and legacy/admin fields which are
// deliberately forbidden in authored manifests. Account topology and hotplug
// bounds too; never turn an unrecognized live object into free capacity.
func liveVMIDemand(object *unstructured.Unstructured) (argoworkflow.VMIDemand, error) {
	fail := errors.New("argojob: cannot bound live VMI demand")
	var live struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Spec       struct {
			Domain struct {
				CPU       struct{ Cores, Sockets, Threads, MaxSockets int }
				Memory    struct{ Guest, MaxGuest string }
				Resources struct{ Requests, Limits struct{ CPU, Memory string } }
			}
		}
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
	if d.CPU.Cores <= 0 || d.CPU.Cores > limit.MaxCores || d.CPU.Sockets < 0 || d.CPU.Sockets > limit.MaxCores || d.CPU.Threads < 0 || d.CPU.Threads > limit.MaxCores || d.CPU.MaxSockets < 0 || d.CPU.MaxSockets > limit.MaxCores {
		return argoworkflow.VMIDemand{}, fail
	}
	cores := d.CPU.Cores * max(1, d.CPU.Sockets, d.CPU.MaxSockets) * max(1, d.CPU.Threads)
	for _, value := range []string{d.Resources.Requests.CPU, d.Resources.Limits.CPU} {
		if value == "" {
			continue
		}
		q, err := resource.ParseQuantity(value)
		if err != nil || q.Sign() <= 0 || q.Cmp(*resource.NewQuantity(int64(limit.MaxCores), resource.DecimalSI)) > 0 {
			return argoworkflow.VMIDemand{}, fail
		}
		cores = max(cores, int(q.Value()))
	}
	var memory int64
	for _, value := range []string{d.Resources.Requests.Memory, d.Resources.Limits.Memory, d.Memory.Guest, d.Memory.MaxGuest} {
		if value == "" {
			continue
		}
		q, err := resource.ParseQuantity(value)
		if err != nil || q.Sign() <= 0 || q.Cmp(limit.MaxMemory) > 0 {
			return argoworkflow.VMIDemand{}, fail
		}
		memory = max(memory, q.Value())
	}
	demand := argoworkflow.VMIDemand{VMs: 1, Cores: cores, MemoryBytes: memory}
	if !validVMIDemand(demand) {
		return argoworkflow.VMIDemand{}, fail
	}
	return demand, nil
}
