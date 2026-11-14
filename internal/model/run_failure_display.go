package model

// WithFailedNode resolves only the stored failure identity, never a same-name sibling.
func (run Run) WithFailedNode(rows []StepResult) Run {
	for i := range rows {
		if rows[i].NodeID == run.FailedStep && (rows[i].RunID == "" || rows[i].RunID == run.ID) {
			row := rows[i]
			run.FailedNode = &row
			break
		}
	}
	return run
}

func (run Run) FailedStepDisplayName() string {
	if run.FailedNode != nil && run.FailedNode.NodeID == run.FailedStep {
		if run.FailedNode.DisplayName != "" {
			return run.FailedNode.DisplayName
		}
		if run.FailedNode.Step != "" {
			return run.FailedNode.Step
		}
	}
	return run.FailedStep
}

func (run Run) FailedStepName() string {
	if run.FailedNode != nil && run.FailedNode.NodeID == run.FailedStep && run.FailedNode.Step != "" {
		return run.FailedNode.Step
	}
	return run.FailedStep
}
