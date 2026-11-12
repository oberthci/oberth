package argojob

import (
	"errors"
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// capCPURequests changes scheduling reservations only; burst limits stay intact.
func capCPURequests(wf *wfv1.Workflow, milli int64) {
	if milli <= 0 {
		return
	}
	cap := func(c *corev1.Container) {
		if c == nil {
			return
		}
		q, ok := c.Resources.Requests[corev1.ResourceCPU]
		if ok && q.MilliValue() > milli {
			c.Resources.Requests[corev1.ResourceCPU] = *resource.NewMilliQuantity(milli, resource.DecimalSI)
		}
	}
	visit := func(t *wfv1.Template) {
		cap(t.Container)
		if t.Script != nil {
			cap(&t.Script.Container)
		}
		if t.ContainerSet != nil {
			for i := range t.ContainerSet.Containers {
				cap(&t.ContainerSet.Containers[i].Container)
			}
		}
		for i := range t.InitContainers {
			cap(&t.InitContainers[i].Container)
		}
		for i := range t.Sidecars {
			cap(&t.Sidecars[i].Container)
		}
	}
	walkTemplate(wf.Spec.TemplateDefaults, visit, 0)
	walkTemplates(wf, visit)
}

// SchedulingDemand counts the effective request graph, including defaults and
// one 50m Argo executor allowance per concurrent Pod. Missing memory limits
// are irrelevant to CPU scheduling; fill them only in this private analysis.
func SchedulingDemand(wf *wfv1.Workflow, cap int64) (*argoworkflow.PodDemand, error) {
	wf = wf.DeepCopy()
	applyRequestDefaultsPolicy(wf, false, false)
	capCPURequests(wf, cap)
	memory := func(c *corev1.Container) {
		if c == nil {
			return
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		if q := c.Resources.Limits[corev1.ResourceMemory]; q.Sign() <= 0 {
			c.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1Ti")
		}
	}
	visit := func(t *wfv1.Template) {
		memory(t.Container)
		if t.Script != nil {
			memory(&t.Script.Container)
		}
		if t.ContainerSet != nil {
			for i := range t.ContainerSet.Containers {
				memory(&t.ContainerSet.Containers[i].Container)
			}
		}
		for i := range t.InitContainers {
			memory(&t.InitContainers[i].Container)
		}
		for i := range t.Sidecars {
			memory(&t.Sidecars[i].Container)
		}
	}
	walkTemplate(wf.Spec.TemplateDefaults, visit, 0)
	walkTemplates(wf, visit)
	d, err := argoworkflow.DeclaredCapabilityPeak(wf)
	if err != nil {
		return nil, fmt.Errorf("workflow scheduling requests cannot be bounded: %w", err)
	}
	if d == nil {
		return nil, errors.New("workflow scheduling requests cannot be bounded")
	}
	d.CPUMilli += d.Pods * 50
	return d, nil
}

func stampSchedulingDemand(workflow *wfv1.Workflow, config Config, request Request) error {
	demand := request.PodDemand
	if config.StepCPURequestMaxMilli > 0 && request.DemandReceipt != nil {
		var err error
		demand, err = SchedulingDemand(request.DemandReceipt.Workflow(), config.StepCPURequestMaxMilli)
		if err != nil {
			return err
		}
	}
	stampPodDemand(workflow, demand)
	return stampResourceFallback(workflow, request.ResourceFallback)
}
