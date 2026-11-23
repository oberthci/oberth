package admissioncontract

type Plan struct {
	Entries []PlanEntry `json:"entries"`
	Exact   bool        `json:"exact"`
}

type PlanEntry struct {
	Path             string `json:"plan_path"`
	Name             string `json:"name"`
	Template         string `json:"-"`
	Dynamic          bool   `json:"dynamic"`
	ParallelGroup    int    `json:"parallel_group,omitempty"`
	CredentialIntent bool   `json:"credential_intent"`
	HoldArmed        bool   `json:"hold_armed,omitempty"`
	HoldEligible     bool   `json:"hold_eligible,omitempty"`
	HoldRefusal      string `json:"hold_refusal,omitempty"`
}
