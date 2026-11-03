package argoworkflow

import (
	"fmt"
	"strconv"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// CleanupVerdictAnnotation opts into a server-side terminal hook. Its value
// is the total cleanup/absence-readback deadline in seconds (1..300).
const CleanupVerdictAnnotation = "oberth.ci/cleanup-verdict"

func CleanupVerdictTimeout(wf *wfv1.Workflow) (time.Duration, error) {
	value, declared := wf.Annotations[CleanupVerdictAnnotation]
	if !declared {
		return 0, nil
	}
	for _, template := range wf.Spec.Templates {
		if template.Name == "oberth-cleanup" {
			return 0, fmt.Errorf("argoworkflow: template name oberth-cleanup is reserved by the cleanup verdict")
		}
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 1 || seconds > 300 || strconv.Itoa(seconds) != value {
		return 0, fmt.Errorf("argoworkflow: %s must be an integer number of seconds in 1..300", CleanupVerdictAnnotation)
	}
	return time.Duration(seconds) * time.Second, nil
}
