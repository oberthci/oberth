package argoworkflow

import (
	"encoding/json"
	"errors"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// VMILauncherCPUMilli reserves VM launcher capacity, separate from workflow
// executor reserve. Account for the launcher without
// modifying the manifest. Deployment overhead must fit this reviewed allowance.
const VMILauncherCPUMilli int64 = 100
const VMILauncherMemoryBytes int64 = 256 << 20

var ErrVMHostUndeclared = errors.New("VM scheduling resources are undeclared")

func vmHostCost(raw string) (string, PodDemand, error) {
	b, err := yaml.YAMLToJSONStrict([]byte(raw))
	if err != nil {
		return "", PodDemand{}, err
	}
	var m vmiManifest
	if err = json.Unmarshal(b, &m); err != nil {
		return "", PodDemand{}, err
	}
	r := m.Spec.Domain.Resources
	cpuText := r.Requests.CPU
	if cpuText == "" {
		cpuText = r.Limits.CPU
	}
	memText := r.Requests.Memory
	if memText == "" {
		memText = r.Limits.Memory
	}
	if cpuText == "" || memText == "" {
		return "", PodDemand{}, ErrVMHostUndeclared
	}
	if r.Limits.Memory == "" {
		return "", PodDemand{}, ErrMemoryUnbounded
	}
	cpu, e := resource.ParseQuantity(cpuText)
	if e != nil || cpu.Sign() <= 0 || cpu.CmpInt64(1000) > 0 {
		return "", PodDemand{}, errPodGraph
	}
	memory, e := resource.ParseQuantity(memText)
	if e != nil || memory.Sign() <= 0 || memory.CmpInt64(1<<40) > 0 {
		return "", PodDemand{}, errPodGraph
	}
	limit, e := resource.ParseQuantity(r.Limits.Memory)
	if e != nil || limit.Sign() <= 0 {
		return "", PodDemand{}, ErrMemoryUnbounded
	}
	if limit.CmpInt64(1<<40) > 0 {
		return "", PodDemand{}, errPodGraph
	}
	if limit.Cmp(memory) > 0 {
		memory = limit
	}
	return m.Metadata.Name, PodDemand{CPUMilli: cpu.MilliValue() + VMILauncherCPUMilli, MemoryBytes: memory.Value() + VMILauncherMemoryBytes}, nil
}

// DeclaredHostPeak adds VM lifetime demand to the workflow pod envelope. The
// dimensions may peak at different times; adding independent upper bounds is
// conservative. Cell quotas continue to use their separate demand receipt.
func DeclaredHostPeak(wf *wfv1.Workflow, trigger RunTrigger) (*PodDemand, error) {
	if !DeclaresVMIResources(wf) {
		return DeclaredPodPeak(wf)
	}
	costs := map[string]PodDemand{}
	var sum PodDemand
	for _, t := range wf.Spec.Templates {
		if t.Resource == nil || t.Resource.Action != "create" {
			continue
		}
		name, cost, err := vmHostCost(t.Resource.Manifest)
		if err != nil {
			return nil, err
		}
		old := costs[name]
		costs[name] = PodDemand{CPUMilli: max(old.CPUMilli, cost.CPUMilli), MemoryBytes: max(old.MemoryBytes, cost.MemoryBytes)}
		sum.CPUMilli += cost.CPUMilli
		sum.MemoryBytes += cost.MemoryBytes
	}
	if len(costs) == 0 {
		return nil, ErrVMHostUndeclared
	}
	main := wf.DeepCopy()
	main.Spec.OnExit = ""
	d, err := declaredPodPeak(main, true)
	if err != nil || d == nil {
		return nil, err
	}
	if wf.Spec.OnExit != "" {
		exit := wf.DeepCopy()
		exit.Spec.Entrypoint = wf.Spec.OnExit
		exit.Spec.OnExit = ""
		x, e := declaredPodPeak(exit, true)
		if e != nil || x == nil {
			return nil, e
		}
		d.CPUMilli += x.CPUMilli
		d.MemoryBytes += x.MemoryBytes
		d.Pods += x.Pods
	}
	if peak, ok := runVMIPeakWithCosts(wf, trigger, costs); ok {
		sum.CPUMilli = min(sum.CPUMilli, int64(peak.Cores))
		sum.MemoryBytes = min(sum.MemoryBytes, peak.MemoryBytes)
	}
	d.CPUMilli += sum.CPUMilli
	d.MemoryBytes += sum.MemoryBytes
	if !d.Valid() {
		return nil, errPodGraph
	}
	return d, nil
}
