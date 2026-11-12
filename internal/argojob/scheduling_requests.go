package argojob

import (
	"context"
	"encoding/json"
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
func SchedulingDemand(wf *wfv1.Workflow, policy argoworkflow.CPURequestPolicy) (*argoworkflow.PodDemand, error) {
	wf = wf.DeepCopy()
	applyRequestDefaultsPolicy(wf, false, false)
	applySchedulingCPU(wf, policy)
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
	helper := executorControlResources().Requests
	d.CPUMilli += d.Pods * helper.Cpu().MilliValue()
	d.MemoryBytes += d.Pods * helper.Memory().Value()
	if d.DeclaredTotal != nil {
		d.DeclaredTotal.CPUMilli += d.DeclaredTotal.Pods * helper.Cpu().MilliValue()
		d.DeclaredTotal.MemoryBytes += d.DeclaredTotal.Pods * helper.Memory().Value()
	}
	if !d.Valid() {
		return nil, errors.New("effective scheduling demand cannot be bounded")
	}
	return d, nil
}

const cpuRequestsAnnotation = "oberth.ci/cpu-requests"

func (request Request) cpuRequestPolicy(config Config) argoworkflow.CPURequestPolicy {
	if request.recoveredCPU != nil {
		return request.recoveredCPU.Clone()
	}
	policy := argoworkflow.CPURequestPolicy{MaxMilli: config.StepCPURequestMaxMilli}
	if p := request.DemandReceipt.CPURequestPolicy(); p != nil {
		policy = *p
	}
	if config.CPUAdmissionDisabled {
		return argoworkflow.CPURequestPolicy{}
	}
	// A pool may have been unavailable during preparation. Resolve its node
	// cap before first submission; a later smaller cap can only lower demand.
	// Recovery above retains the exact policy already submitted.
	if config.CPUCapacity != nil {
		current := config.CurrentCPU(context.Background()).StepMaxMilli
		if current > 0 && (policy.MaxMilli == 0 || current < policy.MaxMilli) {
			policy.MaxMilli = current
		}
	}
	return policy
}

// FreezeSchedulingCPU completes the cached node-cap decision once, before
// submission audit and native creation. It never resamples usage history.
func (request Request) FreezeSchedulingCPU(config Config) Request {
	policy := request.cpuRequestPolicy(config)
	request.recoveredCPU = &policy
	return request
}
func (request *Request) applyCPU(config Config, wf *wfv1.Workflow) {
	*request = request.FreezeSchedulingCPU(config)
	applySchedulingCPU(wf, *request.recoveredCPU)
}

func recoverCPURequestPolicy(wf *wfv1.Workflow, request *Request) error {
	raw := wf.Annotations[cpuRequestsAnnotation]
	if raw == "" {
		return nil
	}
	var p argoworkflow.CPURequestPolicy
	if len(raw) > 32<<10 || json.Unmarshal([]byte(raw), &p) != nil || !p.Valid() {
		return errors.New("invalid CPU request policy binding")
	}
	request.recoveredCPU = &p
	return nil
}
func applySchedulingCPU(wf *wfv1.Workflow, policy argoworkflow.CPURequestPolicy) {
	if policy.Parallelism > 0 && wf.Spec.Parallelism == nil {
		n := policy.Parallelism
		wf.Spec.Parallelism = &n
	}
	visit := func(t *wfv1.Template) {
		milli := policy.Templates[t.Name]
		if milli <= 0 {
			return
		}
		c := t.Container
		if t.Script != nil {
			c = &t.Script.Container
		}
		if c == nil {
			return
		}
		q, ok := c.Resources.Requests[corev1.ResourceCPU]
		if ok {
			declared := q.MilliValue()
			milli = max(milli, max(int64(50), (declared+9)/10))
			if declared > milli {
				c.Resources.Requests[corev1.ResourceCPU] = *resource.NewMilliQuantity(milli, resource.DecimalSI)
			}
		}
	}
	walkTemplates(wf, visit)
	// The operator's node fraction remains the final safety cap, including
	// sidecars and init containers. Limits are never modified.
	capCPURequests(wf, policy.MaxMilli)
}

func stampSchedulingDemand(workflow *wfv1.Workflow, config Config, request Request) error {
	policy := request.cpuRequestPolicy(config)
	if !policy.Valid() {
		return errors.New("invalid CPU request policy")
	}
	delete(workflow.Annotations, cpuRequestsAnnotation)
	demand := request.PodDemand
	if (policy.MaxMilli > 0 || len(policy.Templates) > 0 || policy.Parallelism > 0) && request.DemandReceipt != nil {
		// Unsupported graph shapes reserve the whole admission pool; they remain valid Argo workflows.
		demand, _ = SchedulingDemand(request.DemandReceipt.Workflow(), policy)
		body, _ := json.Marshal(policy)
		workflow.Annotations[cpuRequestsAnnotation] = string(body)
	}
	stampPodDemand(workflow, demand)
	return stampResourceFallback(workflow, request.ResourceFallback)
}

// CurrentCPU reads a cached observation; operator clients refresh independently.
func (config Config) CurrentCPU(ctx context.Context) argoworkflow.CPUAdmission {
	if config.CPUAdmissionDisabled {
		return argoworkflow.CPUAdmission{Pool: config.CPUAdmission.Pool}.Canonical()
	}
	if config.CPUCapacity != nil {
		return config.CPUCapacity(ctx).Canonical()
	}
	return config.CPUAdmission.Canonical()
}
