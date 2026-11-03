package argoworkflow

import (
	"errors"
	"fmt"
	"slices"
	"strconv"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// ResourceCeiling bounds each pipeline container's requests and limits. The
// durable administrator grant selects the ceiling; pipeline size annotations
// and resource history do not select or raise it. VM grants remain separate.
type ResourceCeiling struct {
	CPU              string `json:"cpu"`
	Memory           string `json:"memory"`
	EphemeralStorage string `json:"ephemeral-storage"`
}

func ResourceCeilingForSize(maximum periapsis.Size) (ResourceCeiling, error) {
	switch maximum {
	case periapsis.L:
		return ResourceCeiling{CPU: "4", Memory: "8Gi", EphemeralStorage: "16Gi"}, nil
	case periapsis.XL:
		return ResourceCeiling{CPU: MaxContainerCPU, Memory: MaxContainerMemory, EphemeralStorage: MaxContainerEphemeral}, nil
	default:
		return ResourceCeiling{}, errors.New("invalid administrator resource ceiling grant")
	}
}

// ValidateResourceCeiling reads declarations only. It never defaults, clamps,
// rounds or changes requests/limits. Call before server helper injection.
func ValidateResourceCeiling(wf *wfv1.Workflow, repo string, maximum periapsis.Size, deadlineSeconds int64) error {
	return ValidateResourceCeilingWithCapability(wf, repo, maximum, deadlineSeconds, nil)
}

// ValidateResourceCeilingWithCapability preserves the ordinary CPU/memory
// ceilings while admitting only explicitly granted extended dimensions.
func ValidateResourceCeilingWithCapability(wf *wfv1.Workflow, repo string, maximum periapsis.Size, deadlineSeconds int64, capability *Capability) error {
	if err := capability.Validate(); err != nil {
		return err
	}
	ceiling, err := ResourceCeilingForSize(maximum)
	if err != nil {
		return err
	}
	return validateResourceRequirements(wf, repo, ResourceRequirementsForCeiling(ceiling), string(maximum), deadlineSeconds, capability)
}

// ResourceRequirementsForCeiling is the upgrade mapping from historical grants.
func ResourceRequirementsForCeiling(c ResourceCeiling) corev1.ResourceRequirements {
	values := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(c.CPU), corev1.ResourceMemory: resource.MustParse(c.Memory), corev1.ResourceEphemeralStorage: resource.MustParse(c.EphemeralStorage)}
	return corev1.ResourceRequirements{Requests: values.DeepCopy(), Limits: values.DeepCopy()}
}

// ValidateAdministratorResources checks independent request/limit ceilings. A
// CPU limit ceiling does not require workflows to declare a CPU limit.
func ValidateAdministratorResources(c corev1.ResourceRequirements) error {
	global, _ := ResourceCeilingForSize(periapsis.XL)
	bounds := ResourceRequirementsForCeiling(global)
	if len(c.Claims) != 0 {
		return errors.New("resource ceiling cannot contain claims")
	}
	for _, values := range []corev1.ResourceList{c.Requests, c.Limits} {
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
			q, ok := values[name]
			if !ok || q.Sign() <= 0 || q.Cmp(bounds.Limits[name]) > 0 {
				return fmt.Errorf("administrator resource ceiling requires positive %s within %s", name, bounds.Limits.Name(name, resource.DecimalSI).String())
			}
		}
		if len(values) != 3 {
			return errors.New("administrator resource ceiling supports CPU, memory and ephemeral-storage only")
		}
	}
	for name, request := range c.Requests {
		if request.Cmp(c.Limits[name]) > 0 {
			return fmt.Errorf("administrator requests.%s exceeds limits.%s", name, name)
		}
	}
	return nil
}

func ValidateResourceRequirements(wf *wfv1.Workflow, repo string, ceiling corev1.ResourceRequirements, deadlineSeconds int64) error {
	return ValidateResourceRequirementsWithCapability(wf, repo, ceiling, deadlineSeconds, nil)
}

// ValidateResourceRequirementsWithCapability also enforces operator-owned extended-resource grants.
func ValidateResourceRequirementsWithCapability(wf *wfv1.Workflow, repo string, ceiling corev1.ResourceRequirements, deadlineSeconds int64, capability *Capability) error {
	if err := capability.Validate(); err != nil {
		return err
	}
	if err := ValidateAdministratorResources(ceiling); err != nil {
		return err
	}
	return validateResourceRequirements(wf, repo, ceiling, "resource", deadlineSeconds, capability)
}

func validateResourceRequirements(wf *wfv1.Workflow, repo string, ceiling corev1.ResourceRequirements, maximum string, deadlineSeconds int64, capability *Capability) error {
	ceiling = *ceiling.DeepCopy()
	if capability != nil {
		for name, count := range capability.ExtendedResources {
			quantity := *resource.NewQuantity(count, resource.DecimalSI)
			ceiling.Requests[corev1.ResourceName(name)] = quantity
			ceiling.Limits[corev1.ResourceName(name)] = quantity
		}
	}
	check := func(location string, r corev1.ResourceRequirements) error {
		for _, tier := range []struct {
			name   string
			values corev1.ResourceList
		}{{"requests", r.Requests}, {"limits", r.Limits}} {
			names := make([]corev1.ResourceName, 0, len(tier.values))
			for name := range tier.values {
				names = append(names, name)
			}
			slices.Sort(names)
			for _, name := range names {
				q := tier.values[name]
				bound, ok := ceiling.Requests[name]
				if tier.name == "limits" {
					bound, ok = ceiling.Limits[name]
				}
				if !ok {
					return fmt.Errorf("repo %s %s: resource %s is not admitted", repo, location, name)
				}
				if q.Sign() < 0 {
					return fmt.Errorf("repo %s %s: resources.%s.%s must not be negative", repo, location, tier.name, name)
				}
				if q.Cmp(bound) > 0 {
					return fmt.Errorf("repo %s %s: resources.%s.%s %s exceeds administrator %s ceiling %s; an administrator must change the resource grant, or reduce the declaration in git", repo, location, tier.name, name, q.String(), maximum, bound.String())
				}
			}
		}
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
			request, requested := r.Requests[name]
			limit, limited := r.Limits[name]
			if requested && limited && request.Cmp(limit) > 0 {
				return fmt.Errorf("repo %s %s: resources.requests.%s %s exceeds declared limit %s", repo, location, name, request.String(), limit.String())
			}
		}
		return nil
	}
	var walk func(*wfv1.Template, string, int) error
	walk = func(t *wfv1.Template, location string, depth int) error {
		if t == nil {
			return nil
		}
		if depth > MaxIdentityWalkDepth {
			return errors.New("resource declaration nesting exceeds bound")
		}
		if t.ActiveDeadlineSeconds != nil {
			n, e := strconv.ParseInt(t.ActiveDeadlineSeconds.String(), 10, 64)
			if e != nil || n <= 0 {
				return fmt.Errorf("repo %s %s: activeDeadlineSeconds must be a positive integer", repo, location)
			}
			if n > deadlineSeconds {
				return fmt.Errorf("repo %s %s: activeDeadlineSeconds %d exceeds administrator deadline ceiling %d", repo, location, n, deadlineSeconds)
			}
		}
		if t.Container != nil {
			if e := check(location, t.Container.Resources); e != nil {
				return e
			}
		}
		if t.Script != nil {
			if e := check(location, t.Script.Resources); e != nil {
				return e
			}
		}
		if t.ContainerSet != nil {
			for i := range t.ContainerSet.Containers {
				c := &t.ContainerSet.Containers[i]
				if e := check(fmt.Sprintf("%s container %s", location, c.Name), c.Resources); e != nil {
					return e
				}
			}
		}
		for _, group := range []struct {
			name       string
			containers []wfv1.UserContainer
		}{{"init", t.InitContainers}, {"sidecar", t.Sidecars}} {
			for i, c := range group.containers {
				if e := check(fmt.Sprintf("%s %s[%d]", location, group.name, i), c.Resources); e != nil {
					return e
				}
			}
		}
		for _, call := range invocations(t) {
			if call.inline != nil {
				if e := walk(call.inline, location+" inline "+call.name, depth+1); e != nil {
					return e
				}
			}
		}
		return nil
	}
	if wf == nil {
		return errors.New("resource declaration workflow missing")
	}
	if err := walk(wf.Spec.TemplateDefaults, "templateDefaults", 0); err != nil {
		return err
	}
	for i := range wf.Spec.Templates {
		t := &wf.Spec.Templates[i]
		if err := walk(t, "template "+t.Name, 0); err != nil {
			return err
		}
	}
	return nil
}

type ResourceDeclarationCoverage struct{ Declared, Undeclared int }

// ResourceCoverage counts reachable pod-producing call sites, once per declared
// fan-out site (not per runtime item or retry). A root pod counts too. Unknown
// graphs stay unknown; unreferenced utility templates do not inflate the count.
// Completeness means every pipeline main/container-set container explicitly
// requests positive CPU and memory, including inherited template defaults.
func ResourceCoverage(wf *wfv1.Workflow) *ResourceDeclarationCoverage {
	if wf == nil {
		return nil
	}
	templates := map[string]*wfv1.Template{}
	for i := range wf.Spec.Templates {
		t := &wf.Spec.Templates[i]
		templates[t.Name] = t
	}
	out := &ResourceDeclarationCoverage{}
	active := map[*wfv1.Template]bool{}
	visited := 0
	complete := func(c *corev1.Container, defaults *corev1.Container) bool {
		if c == nil {
			return false
		}
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			q, ok := c.Resources.Requests[name]
			if !ok && defaults != nil {
				q, ok = defaults.Resources.Requests[name]
			}
			if !ok || q.Sign() <= 0 {
				return false
			}
		}
		return true
	}
	var walk func(*wfv1.Template, int) bool
	walk = func(t *wfv1.Template, depth int) bool {
		if t == nil || depth > MaxIdentityWalkDepth || active[t] || visited >= MaxPlannedSteps*MaxIdentityWalkDepth {
			return false
		}
		visited++
		active[t] = true
		defer delete(active, t)
		if podProducing(t) {
			var defaults *corev1.Container
			if d := wf.Spec.TemplateDefaults; d != nil {
				if d.Container != nil {
					defaults = d.Container
				} else if d.Script != nil {
					defaults = &d.Script.Container
				}
			}
			declared := false
			switch {
			case t.Container != nil:
				declared = complete(t.Container, defaults)
			case t.Script != nil:
				declared = complete(&t.Script.Container, defaults)
			case t.ContainerSet != nil:
				declared = len(t.ContainerSet.Containers) > 0
				for i := range t.ContainerSet.Containers {
					declared = complete(&t.ContainerSet.Containers[i].Container, nil) && declared
				}
			}
			if declared {
				out.Declared++
			} else {
				out.Undeclared++
			}
			if out.Declared+out.Undeclared > MaxPlannedSteps {
				return false
			}
		}
		for _, call := range invocations(t) {
			target := call.inline
			if target == nil {
				target = templates[call.template]
			}
			if !walk(target, depth+1) {
				return false
			}
		}
		if t.DAG != nil {
			for _, task := range t.DAG.Tasks {
				for _, hook := range task.Hooks {
					if !walk(templates[hook.Template], depth+1) {
						return false
					}
				}
			}
		}
		for _, group := range t.Steps {
			for _, step := range group.Steps {
				for _, hook := range step.Hooks {
					if !walk(templates[hook.Template], depth+1) {
						return false
					}
				}
			}
		}
		return true
	}
	if !walk(templates[wf.Spec.Entrypoint], 0) {
		return nil
	}
	if wf.Spec.OnExit != "" && !walk(templates[wf.Spec.OnExit], 0) {
		return nil
	}
	for _, hook := range wf.Spec.Hooks {
		if !walk(templates[hook.Template], 0) {
			return nil
		}
	}
	return out
}
