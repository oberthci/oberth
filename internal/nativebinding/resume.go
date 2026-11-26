package nativebinding

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
)

// ResumePod names one retry deletion, fenced by the UID read at planning.
type ResumePod struct {
	Name string
	UID  types.UID
}

// ResumePlan is the durable write intent accepted before any retry effect.
type ResumePlan struct {
	Workflow                        *wf.Workflow
	UID                             types.UID
	ResourceVersion                 string
	ObservedPods                    []ResumePod
	Pods                            []ResumePod
	Carried, Remaining, SecretSteps []string
}
