package sourcedelivery

import "errors"

// Budgets is the single startup tuple for source loading. It does not control
// submission, native workflow scheduling or the separate secret broker.
type Budgets struct {
	Exports         int `json:"exports"`
	Archives        int `json:"archives"`
	Transfers       int `json:"transfers"`
	Auth            int `json:"auth"`
	WorkflowWaiters int `json:"workflow_waiters"`
}

func DefaultBudgets() Budgets {
	return Budgets{Exports: 1, Archives: 2, Transfers: 2, Auth: 32, WorkflowWaiters: 16}
}

func ResolveBudgets(input *Budgets) (Budgets, error) {
	if input == nil {
		return DefaultBudgets(), nil
	}
	b := *input
	for _, limit := range []struct{ value, ceiling int }{{b.Exports, 8}, {b.Archives, 32}, {b.Transfers, 32}, {b.Auth, 128}, {b.WorkflowWaiters, 16}} {
		if limit.value < 1 || limit.value > limit.ceiling {
			return b, errors.New("invalid source delivery budgets")
		}
	}
	if b.Exports > b.Archives || b.Exports > b.Transfers {
		return b, errors.New("invalid source delivery budgets")
	}
	return b, nil
}
