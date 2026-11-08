package argojob

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// DefaultExecutionTimeout is the default Config.DefaultWorkflowTimeout: the
// deadline of a workflow that omits its own.
const DefaultExecutionTimeout = 30 * time.Minute

// ExecutionTimeout applies the administrator's global and canonical repository
// caps. A repository document may shorten these limits, never raise them.
func (config Config) ExecutionTimeout(repository string, declared *int64) time.Duration {
	config.applyDefaults()
	limit := config.WorkflowTimeout
	if cap := config.PerRepoWorkflowTimeout[repository]; cap > 0 && cap < limit {
		limit = cap
	}
	duration := config.DefaultWorkflowTimeout
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

// StepStartAllowanceAnnotation records the start-up allowance Build added to
// the declared step deadline, so classification never depends on the server's
// current configuration. Server-owned exactly like StepDeadlineAnnotation.
const StepStartAllowanceAnnotation = "oberth.ci/step-start-allowance-seconds"

// Administrator bounds for StepStartupAllowance.
const (
	MaxStepStartupAllowancePercent = 400
	MaxStepStartupAllowanceSeconds = 3600
)

// StepStartupAllowance is the bounded pre-start time a step Pod receives on
// top of its declared deadline (#1499). Kubernetes counts
// activeDeadlineSeconds from Pod start, so image pulls, init containers and
// kubelet contention would otherwise be charged to the step command. The
// allowance is FactorPercent of the declared budget, clamped to
// [MinSeconds, MaxSeconds]. An allowance of zero (MaxSeconds 0, or both
// FactorPercent and MinSeconds 0) leaves step deadlines exactly as declared.
type StepStartupAllowance struct {
	FactorPercent int64
	MinSeconds    int64
	MaxSeconds    int64
}

// DefaultStepStartupAllowance is half the declared budget, at least two and
// at most ten minutes: max(1.5x, x+120s), capped at x+600s.
var DefaultStepStartupAllowance = StepStartupAllowance{FactorPercent: 50, MinSeconds: 120, MaxSeconds: 600}

// Validate bounds an administrator allowance.
func (allowance StepStartupAllowance) Validate() error {
	if allowance.FactorPercent < 0 || allowance.FactorPercent > MaxStepStartupAllowancePercent {
		return fmt.Errorf("argojob: step startup allowance percent must be 0 to %d", MaxStepStartupAllowancePercent)
	}
	if allowance.MinSeconds < 0 || allowance.MaxSeconds < 0 || allowance.MaxSeconds > MaxStepStartupAllowanceSeconds {
		return fmt.Errorf("argojob: step startup allowance seconds must be 0 to %d", MaxStepStartupAllowanceSeconds)
	}
	if allowance.MinSeconds > allowance.MaxSeconds {
		return errors.New("argojob: step startup allowance minimum must not exceed its maximum")
	}
	return nil
}

// Seconds is the allowance for one declared step deadline. Overflow-safe for
// every int64: the proportional part is computed only below the maximum.
func (allowance StepStartupAllowance) Seconds(declared int64) int64 {
	if declared <= 0 || declared > (1<<62) || allowance.MaxSeconds <= 0 {
		return 0
	}
	seconds := allowance.MaxSeconds
	if allowance.FactorPercent == 0 {
		seconds = 0
	} else if declared <= allowance.MaxSeconds*100/allowance.FactorPercent+1 {
		seconds = min(declared*allowance.FactorPercent/100, allowance.MaxSeconds)
	}
	return max(seconds, allowance.MinSeconds)
}

// Floored is the Pod deadline Build submits for a declared step deadline.
// Overflow-safe for every admitted value.
func (allowance StepStartupAllowance) Floored(declared int64) int64 {
	return declared + allowance.Seconds(declared)
}

// applyStepDeadlineFloor widens every literal leaf deadline by the pre-start
// allowance and records the declared budget on the step Pod. The declared
// value stays the step's verdict budget: a deadline kill whose main container
// ran past it is still the step's own failure. Non-literal values are left for
// Argo to reject, exactly as before. The workflow deadline is unchanged and
// still bounds the whole run.
func applyStepDeadlineFloor(workflow *wfv1.Workflow, allowance StepStartupAllowance) {
	var walk func(*wfv1.Template, int)
	walk = func(template *wfv1.Template, depth int) {
		if template == nil || depth > argoworkflow.MaxIdentityWalkDepth {
			return
		}
		delete(template.Metadata.Annotations, StepDeadlineAnnotation)
		delete(template.Metadata.Annotations, StepStartAllowanceAnnotation)
		if template.ActiveDeadlineSeconds != nil && template.DAG == nil && len(template.Steps) == 0 {
			declared, err := strconv.ParseInt(template.ActiveDeadlineSeconds.String(), 10, 64)
			if extra := allowance.Seconds(declared); err == nil && declared > 0 && extra > 0 {
				deadline := allowance.Floored(declared)
				floored := intstr.FromString(strconv.FormatInt(deadline, 10))
				if template.ActiveDeadlineSeconds.Type == intstr.Int && deadline <= 1<<31-1 {
					floored = intstr.FromInt32(int32(deadline))
				}
				template.ActiveDeadlineSeconds = &floored
				if template.Metadata.Annotations == nil {
					template.Metadata.Annotations = map[string]string{}
				}
				template.Metadata.Annotations[StepDeadlineAnnotation] = strconv.FormatInt(declared, 10)
				template.Metadata.Annotations[StepStartAllowanceAnnotation] = strconv.FormatInt(extra, 10)
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
// still carries exactly the floored deadline Build submitted (declared budget
// plus the recorded allowance, independent of the current configuration). Argo lowers a
// Pod deadline to the remaining workflow budget; such a Pod is bound by the
// workflow deadline instead and has no separate step budget here.
func declaredStepDeadline(pod *corev1.Pod) (int64, bool) {
	if pod == nil || pod.Spec.ActiveDeadlineSeconds == nil {
		return 0, false
	}
	raw, ok := pod.Annotations[StepDeadlineAnnotation]
	rawAllowance, allowed := pod.Annotations[StepStartAllowanceAnnotation]
	if !ok || !allowed {
		return 0, false
	}
	declared, err := strconv.ParseInt(raw, 10, 64)
	extra, allowanceErr := strconv.ParseInt(rawAllowance, 10, 64)
	if err != nil || allowanceErr != nil || declared <= 0 || declared > (1<<62) || strconv.FormatInt(declared, 10) != raw ||
		extra <= 0 || extra > MaxStepStartupAllowanceSeconds || strconv.FormatInt(extra, 10) != rawAllowance || declared+extra != *pod.Spec.ActiveDeadlineSeconds {
		return 0, false
	}
	return declared, true
}
