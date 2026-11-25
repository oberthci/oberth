package runlog

import (
	"context"
	"slices"
)

// InfrastructureReceipt is a server-observed delivery failure, never step output.
// RunID, workflow, node, path and physical Pod identity are verified by the caller.
type InfrastructureReceipt struct {
	RunID       string `json:"run_id"`
	WorkflowUID string `json:"workflow_uid"`
	NodeID      string `json:"node_id"`
	Step        string `json:"step"`
	PodUID      string `json:"pod_uid"`
	PodRestarts int32  `json:"pod_restarts,omitempty"`
	Kind        string `json:"kind"`
}

func (r InfrastructureReceipt) valid() bool {
	return validID(r.RunID) && validID(r.WorkflowUID) && validID(r.NodeID) && validID(r.PodUID) && r.PodRestarts >= 0 && len(r.Step) <= 4096 && (r.Kind == "source" || r.Kind == "secret")
}

// RecordInfrastructure atomically publishes an idempotent receipt in the synced
// run index. It needs no database connection and shares the log writer gate.
func (s *Store) RecordInfrastructure(ctx context.Context, r InfrastructureReceipt) error {
	if !r.valid() {
		return checkID("")
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	saved, err := s.load(r.RunID)
	if err != nil {
		return err
	}
	if slices.Contains(saved.Infrastructure, r) {
		return nil
	}
	saved.Infrastructure = append(saved.Infrastructure, r)
	return s.save(r.RunID, saved)
}

// Infrastructure returns detached server receipts from the run's durable index.
func (s *Store) Infrastructure(ctx context.Context, id string) ([]InfrastructureReceipt, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.unlock()
	saved, err := s.load(id)
	return saved.Infrastructure, err
}
