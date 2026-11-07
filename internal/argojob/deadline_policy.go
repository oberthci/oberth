package argojob

import (
	"strconv"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// DefaultExecutionTimeout bounds workflows that omit a repository deadline.
const DefaultExecutionTimeout = 30 * time.Minute

// ExecutionTimeout applies the administrator's global and canonical repository
// caps. A repository document may shorten these limits, never raise them.
func (config Config) ExecutionTimeout(repository string, declared *int64) time.Duration {
	config.applyDefaults()
	limit := config.WorkflowTimeout
	if cap := config.PerRepoWorkflowTimeout[repository]; cap > 0 && cap < limit {
		limit = cap
	}
	duration := DefaultExecutionTimeout
	if declared != nil && *declared > 0 {
		// Compare seconds before multiplying so untrusted int64 values cannot overflow.
		if *declared >= int64(limit/time.Second) {
			return limit
		}
		duration = time.Duration(*declared) * time.Second
	}
	if duration > limit {
		duration = limit
	}
	return duration
}

func applyExecutionDeadline(workflow *wfv1.Workflow, config Config, repository string) {
	// Preserve invalid explicit values for admission to reject.
	if workflow.Spec.ActiveDeadlineSeconds != nil && *workflow.Spec.ActiveDeadlineSeconds <= 0 {
		return
	}
	seconds := int64(config.ExecutionTimeout(repository, workflow.Spec.ActiveDeadlineSeconds) / time.Second)
	workflow.Spec.ActiveDeadlineSeconds = &seconds
}

// StepDeadlineAnnotation is the server-owned Pod annotation that records the
// step deadline the document declared. Admission refuses repository-authored
// template annotations outside its allowlist, so only Build writes it.
const StepDeadlineAnnotation = "oberth.ci/step-deadline-seconds"

const (
	minStepStartAllowance = 120 * time.Second
	maxStepStartAllowance = 600 * time.Second
)

// StepStartAllowance is the bounded pre-start time a step Pod receives on top
// of its declared deadline (#1499). Kubernetes counts activeDeadlineSeconds from
// Pod start, so image pulls, init containers and kubelet contention would
// otherwise be charged to the step command. The allowance is half the declared
// budget, at least two and at most ten minutes: max(1.5x, x+120s), capped.
func StepStartAllowance(declared int64) int64 {
	allowance := declared / 2
	if allowance < int64(minStepStartAllowance/time.Second) {
		allowance = int64(minStepStartAllowance / time.Second)
	}
	if allowance > int64(maxStepStartAllowance/time.Second) {
		allowance = int64(maxStepStartAllowance / time.Second)
	}
	return allowance
}

// FlooredStepDeadline is the Pod deadline Build submits for a declared step
// deadline. Overflow-safe for every admitted value.
func FlooredStepDeadline(declared int64) int64 {
	if declared <= 0 || declared > (1<<62) {
		return declared
	}
	return declared + StepStartAllowance(declared)
}

// applyStepDeadlineFloor widens every literal leaf deadline by the pre-start
// allowance and records the declared budget on the step Pod. The declared
// value stays the step's verdict budget: a deadline kill whose main container
// ran past it is still the step's own failure. Non-literal values are left for
// Argo to reject, exactly as before. The workflow deadline is unchanged and
// still bounds the whole run.
func applyStepDeadlineFloor(workflow *wfv1.Workflow) {
	var walk func(*wfv1.Template, int)
	walk = func(template *wfv1.Template, depth int) {
		if template == nil || depth > argoworkflow.MaxIdentityWalkDepth {
			return
		}
		delete(template.Metadata.Annotations, StepDeadlineAnnotation)
		if template.ActiveDeadlineSeconds != nil && template.DAG == nil && len(template.Steps) == 0 {
			declared, err := strconv.ParseInt(template.ActiveDeadlineSeconds.String(), 10, 64)
			if err == nil && declared > 0 {
				floored := intstr.FromString(strconv.FormatInt(FlooredStepDeadline(declared), 10))
				if template.ActiveDeadlineSeconds.Type == intstr.Int && FlooredStepDeadline(declared) <= 1<<31-1 {
					floored = intstr.FromInt32(int32(FlooredStepDeadline(declared)))
				}
				template.ActiveDeadlineSeconds = &floored
				if template.Metadata.Annotations == nil {
					template.Metadata.Annotations = map[string]string{}
				}
				template.Metadata.Annotations[StepDeadlineAnnotation] = strconv.FormatInt(declared, 10)
			}
		}
		for group := range template.Steps {
			for step := range template.Steps[group].Steps {
				walk(template.Steps[group].Steps[step].Inline, depth+1)
			}
		}
		if template.DAG != nil {
			for task := range template.DAG.Tasks {
				walk(template.DAG.Tasks[task].Inline, depth+1)
			}
		}
	}
	for index := range workflow.Spec.Templates {
		walk(&workflow.Spec.Templates[index], 0)
	}
	walk(workflow.Spec.TemplateDefaults, 0)
}

// declaredStepDeadline returns the step budget recorded by Build when the Pod
// still carries exactly the floored deadline Build submitted. Argo lowers a
// Pod deadline to the remaining workflow budget; such a Pod is bound by the
// workflow deadline instead and has no separate step budget here.
func declaredStepDeadline(pod *corev1.Pod) (int64, bool) {
	if pod == nil || pod.Spec.ActiveDeadlineSeconds == nil {
		return 0, false
	}
	raw, ok := pod.Annotations[StepDeadlineAnnotation]
	if !ok {
		return 0, false
	}
	declared, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || declared <= 0 || strconv.FormatInt(declared, 10) != raw || FlooredStepDeadline(declared) != *pod.Spec.ActiveDeadlineSeconds {
		return 0, false
	}
	return declared, true
}
