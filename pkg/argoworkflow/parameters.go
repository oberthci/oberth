package argoworkflow

import (
	"strings"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

type stampedParameter struct {
	name, value, placeholder string
	present                  bool
}

// Parameter names, presence and offline values have one owner for every caller.
func (g *graph) stampedParameters(r admissioncontract.PrepareRequest) []stampedParameter {
	parent := admissioncontract.ScheduleParent(r.Mode, r.Trigger)
	locator := r.RunID
	if parent {
		locator = sourcecontract.ScheduledLocator
	}
	return []stampedParameter{
		{"oberth-ref", r.Ref, "refs/heads/unverified", parent},
		{"oberth-sha", r.CommitSHA, strings.Repeat("0", 40), parent},
		{"oberth-schedule", r.ScheduleEntry, "", r.ScheduleEntry != ""},
		{"oberth-secretstore-address", g.policy.SecretstoreAddress, "https://secretstore.invalid", g.policy.SecretstoreAddress != "" || g.offlineValidation},
		{"oberth-source", locator, strings.Repeat("0", 32), g.inventory.usesSource},
		{"oberth-trigger", string(r.Trigger), "branch", true},
	}
}

func (g *graph) stampParameters(w *wf.Workflow, r admissioncontract.PrepareRequest) {
	for _, p := range g.stampedParameters(r) {
		if !p.present {
			continue
		}
		value := p.value
		if value == "" && g.offlineValidation {
			value = p.placeholder
		}
		w.Spec.Arguments.Parameters = append(w.Spec.Arguments.Parameters, wf.Parameter{Name: p.name, Value: wf.AnyStringPtr(value)})
	}
}

func (g *graph) unknownRole() bool {
	selected, _, _ := workflowpolicy.Select(nil, workflowpolicy.SelectionRequest{Repository: g.request.Repository, File: g.request.SelectedFile})
	return selected == ""
}
