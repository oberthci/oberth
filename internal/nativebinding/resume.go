package nativebinding

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"k8s.io/apimachinery/pkg/types"
	"time"
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
	InPod                           *ResumeHold `json:",omitempty"`
}

// ResumeHold binds the one held attempt and the complete F export. No command
// or agent edits are accepted; the helper retains the original authored argv.
type ResumeHold struct {
	RunID                         string
	PodName, PodUID, NodeID, Step string
	OriginalExit                  int
	HeldSince                     time.Time
	Digest                        string
	Removed                       []string
	Source                        sourcecontract.Binding
	DeadlineRemainingMS           int64
	RetryAdvised                  bool
}

func RerunMarker(commit, run string) string {
	return "oberth-hold: rerunning on " + commit[:min(8, len(commit))] + " as " + run
}

func (p ResumePlan) Mode() string {
	if p.InPod != nil {
		return "in-pod"
	}
	return "retry"
}
