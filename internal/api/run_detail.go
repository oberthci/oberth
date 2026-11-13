package api

import "github.com/oberthci/oberth/internal/model"

// RunDetailResponse is the read-only dashboard view of one run: the durable
// run record, its recorded burn/step results, and the owning repository. It
// intentionally mirrors the raw /api/runs list encoding (Go field names) so
// the dashboard reads one consistent shape.
type RunDetailResponse struct {
	Timing     *model.RunTiming      `json:"Timing,omitempty"`
	Purges     []model.EvidencePurge `json:"Purges,omitempty"`
	Scheduling *model.RunScheduling  `json:"Scheduling,omitempty"`
	Repository model.Repository
	Run        model.Run
	Steps      []model.StepResult
	// RerunOf names the run this one re-ran with run_rerun (#1098).
	RerunOf string `json:"RerunOf,omitempty"`
	// RetryHint classifies a run that did not pass as infrastructure or
	// code (#1514), so an agent can retry without reading logs.
	RetryHint *model.RetryHint `json:"retry_hint,omitempty"`
}

// Shadow only unused diagnostic fields on the HTTP projection. The embedded
// response and step values remain intact for MCP and other readers.
type dashboardStep struct {
	model.StepResult
	Termination any `json:"Termination,omitempty"`
}

func dashboardRunDetail(value RunDetailResponse) any {
	steps := make([]dashboardStep, len(value.Steps))
	for i, step := range value.Steps {
		steps[i].StepResult = step
	}
	return struct {
		RunDetailResponse
		Steps  []dashboardStep
		Timing any `json:"Timing,omitempty"`
	}{RunDetailResponse: value, Steps: steps}
}
