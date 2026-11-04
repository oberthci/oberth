package model

import (
	"encoding/json"
	"fmt"
	"slices"
)

// RunStatusFilter accepts one display status or a list on the wire.
type RunStatusFilter []string

func (statuses RunStatusFilter) Validate() error {
	if len(statuses) > 10 {
		return fmt.Errorf("status accepts at most 10 display statuses")
	}
	for _, status := range statuses {
		if !slices.Contains(DisplayStatuses, status) {
			return fmt.Errorf("unknown run display status %q", status)
		}
	}
	return nil
}

func (statuses *RunStatusFilter) UnmarshalJSON(raw []byte) error {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil && string(raw) != "null" {
		*statuses = RunStatusFilter{one}
	} else {
		var many []string
		if err := json.Unmarshal(raw, &many); err != nil {
			return fmt.Errorf("status must be a display status or list: %w", err)
		}
		if len(many) == 0 {
			return fmt.Errorf("status list must not be empty")
		}
		*statuses = many
	}
	return statuses.Validate()
}
