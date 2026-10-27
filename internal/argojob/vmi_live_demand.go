package argojob

import (
	"encoding/json"
	"errors"
	"math"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

var errLiveVMIOverLimit = errors.New("argojob: live VMI current demand exceeds the accounting bound")

// Live VMIs include KubeVirt defaults and legacy/admin fields forbidden in
// authored manifests. Count current topology and max(guest, requested memory),
// never hotplug ceilings or launcher limits. Read fields independently: a type
// error must not hide readable dimensions. Ambiguous counts contribute at least
// one to the topology product; callers still apply a trusted claim floor or the
// conservative unclaimed fallback. Oversized readable values are retained with
// an over-limit error, saturating arithmetic rather than wrapping.
func liveVMIDemand(object *unstructured.Unstructured) (argoworkflow.VMIDemand, error) {
	fail := errors.New("argojob: cannot bound live VMI demand")
	if object == nil {
		return argoworkflow.VMIDemand{}, fail
	}
	demand := argoworkflow.VMIDemand{VMs: 1, Cores: 1}
	ambiguous := object.GetAPIVersion() != "kubevirt.io/v1" || object.GetKind() != "VirtualMachineInstance"
	for _, name := range []string{"cores", "sockets", "threads"} {
		raw, found, err := unstructured.NestedFieldNoCopy(object.Object, "spec", "domain", "cpu", name)
		if err != nil || !found || raw == nil {
			// Absent sockets/threads default to one. Missing cores, explicit
			// nulls and malformed parents require conservative accounting.
			ambiguous = ambiguous || err != nil || name == "cores" || found
			continue
		}
		count, valid := liveVMICPUCount(raw)
		ambiguous = ambiguous || !valid
		if demand.Cores > math.MaxInt/count {
			demand.Cores = math.MaxInt
		} else {
			demand.Cores *= count
		}
	}
	limit := argoworkflow.VMIBudgetLimit()
	over := demand.Cores > limit.MaxCores
	for _, path := range [][]string{
		{"spec", "domain", "memory", "guest"},
		{"spec", "domain", "resources", "requests", "memory"},
	} {
		raw, found, err := unstructured.NestedFieldNoCopy(object.Object, path...)
		if err != nil {
			ambiguous = true
			continue
		}
		if !found {
			continue
		}
		// Decode only this quantity. A bad sibling (or CPU field) must not
		// prevent this value from contributing to the readable lower bound.
		body, err := json.Marshal(raw)
		var quantity resource.Quantity
		if err != nil || quantity.UnmarshalJSON(body) != nil || quantity.Sign() <= 0 {
			ambiguous = true
			continue
		}
		over = over || quantity.Cmp(limit.MaxMemory) > 0
		memory := int64(math.MaxInt64)
		if quantity.Cmp(*resource.NewQuantity(math.MaxInt64, resource.DecimalSI)) < 0 {
			memory = quantity.Value()
		}
		demand.MemoryBytes = max(demand.MemoryBytes, memory)
	}
	if over {
		return demand, errLiveVMIOverLimit
	}
	if ambiguous || !validVMIDemand(demand) {
		return demand, fail
	}
	return demand, nil
}

// Unstructured JSON is represented with int64 or float64 counts; json.Number
// also occurs with UseNumber decoders. An invalid/missing count cannot erase
// other topology factors. Saturate large numeric values before converting to
// int; the caller marks the resulting product over-limit.
func liveVMICPUCount(raw any) (int, bool) {
	switch count := raw.(type) {
	case int64:
		if count > 0 {
			return int(min(count, int64(math.MaxInt))), true
		}
	case float64:
		if count > 0 && math.Trunc(count) == count {
			if count >= float64(math.MaxInt) {
				return math.MaxInt, true
			}
			return int(count), true
		}
	case json.Number:
		if value, err := count.Int64(); err == nil {
			return liveVMICPUCount(value)
		}
		if value, err := count.Float64(); err == nil || math.IsInf(value, 1) {
			return liveVMICPUCount(value)
		}
	}
	return 1, false
}
