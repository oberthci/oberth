package model

import "encoding/json"

// MarshalJSON omits an unobserved native exit without hiding a known zero.
func (step StepResult) MarshalJSON() ([]byte, error) {
	type fields StepResult
	var exit *int
	if !step.ExitCodeUnknown {
		exit = &step.ExitCode
	}
	step.Message = SanitizeNodeMessage(step.Message)
	return json.Marshal(struct {
		fields
		ExitCode *int `json:"ExitCode,omitempty"`
	}{fields: fields(step), ExitCode: exit})
}

func (step *StepResult) UnmarshalJSON(body []byte) error {
	type fields StepResult
	var value struct {
		fields
		ExitCode *int `json:"ExitCode"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return err
	}
	*step = StepResult(value.fields)
	step.ExitCodeUnknown = value.ExitCode == nil
	if value.ExitCode != nil {
		step.ExitCode = *value.ExitCode
	}
	return nil
}
