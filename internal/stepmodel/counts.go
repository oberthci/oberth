package stepmodel

import "fmt"

// InvalidStatusError refuses an unclassified logical step rather than guessing an outcome.
type InvalidStatusError struct {
	Path   string
	Status StepStatus
}

func (e *InvalidStatusError) Error() string {
	return e.Reason() + ". " + e.Remedy() + fmt.Sprintf(" Step %q has status %q.", e.Path, e.Status)
}

func (e *InvalidStatusError) Reason() string {
	return "Step status is invalid"
}

func (e *InvalidStatusError) Remedy() string {
	return "Project a declared step status; use unknown when native evidence cannot establish an outcome."
}

// CountSteps counts the full logical projection, before paging. The projector supplies
// exactness after expanding dynamic entries and resolving terminal remainders to unknown.
// Invalid statuses return no partial counts. Attempts and display names do not affect totals.
func CountSteps(steps []RunStep, exact bool) (LogicalStepCounts, error) {
	counts := LogicalStepCounts{Total: len(steps), Exact: exact}
	for _, step := range steps {
		if err := counts.add(step); err != nil {
			return LogicalStepCounts{}, err
		}
	}
	return counts, nil
}

func (c *LogicalStepCounts) add(step RunStep) error {
	switch step.Status {
	case StepPending:
		c.Pending++
	case StepQueued:
		c.Queued++
	case StepRunning:
		c.Running++
	case StepPassed:
		c.Passed++
	case StepFailed:
		c.Failed++
	case StepSkipped:
		c.Skipped++
	case StepUnknown:
		c.Unknown++
	default:
		return &InvalidStatusError{Path: step.Path, Status: step.Status}
	}
	return nil
}
