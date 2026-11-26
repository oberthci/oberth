package admissioncontract

// AssistEnabled is the single dormant gate, shared by admission and resume.
// C5's enablement conditions must close before this can change.
const AssistEnabled = false

type Plan struct {
	Entries []PlanEntry `json:"entries"`
	Exact   bool        `json:"exact"`
}

type PlanEntry struct {
	Path             string `json:"plan_path"`
	Name             string `json:"name"`
	Template         string `json:"-"`
	FanOut           bool   `json:"fan_out,omitempty"`
	Dynamic          bool   `json:"dynamic"`
	ParallelGroup    int    `json:"parallel_group,omitempty"`
	CredentialIntent bool   `json:"credential_intent"`
	SecretRoute      bool   `json:"secret_route,omitempty"`
	OutputParameters bool   `json:"output_parameters,omitempty"`
	ResultConsumed   bool   `json:"result_consumed,omitempty"`
	HoldArmed        bool   `json:"hold_armed,omitempty"`
	HoldEligible     bool   `json:"hold_eligible,omitempty"`
	HoldRefusal      string `json:"hold_refusal,omitempty"`
}
