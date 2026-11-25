package runlog

import (
	"context"
	"time"
)

// InfrastructureReceipt is a server-observed terminal failure or superseding
// successful delivery, never step output.
// RunID, workflow, node, path and physical Pod identity are verified by the caller.
type InfrastructureReceipt struct {
	RunID       string    `json:"run_id"`
	WorkflowUID string    `json:"workflow_uid"`
	NodeID      string    `json:"node_id"`
	Step        string    `json:"step"`
	PodUID      string    `json:"pod_uid"`
	PodRestarts int32     `json:"pod_restarts,omitempty"`
	Kind        string    `json:"kind"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	Delivered   bool      `json:"delivered,omitempty"`
}

func (r InfrastructureReceipt) valid() bool {
	return validID(r.RunID) && validID(r.WorkflowUID) && validID(r.NodeID) && validID(r.PodUID) && r.PodRestarts >= 0 && len(r.Step) <= 4096 && (r.Kind == "source" || r.Kind == "secret")
}

func (r InfrastructureReceipt) sameDelivery(other InfrastructureReceipt) bool {
	return r.RunID == other.RunID && r.WorkflowUID == other.WorkflowUID && r.NodeID == other.NodeID && r.Step == other.Step && r.PodUID == other.PodUID && r.PodRestarts == other.PodRestarts && r.Kind == other.Kind
}

// RecordInfrastructure atomically retains the latest completion of each kind in
// the synced run index. It needs no database connection and shares the log gate.
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
	for i, old := range saved.Infrastructure {
		if old.sameDelivery(r) {
			// Persist completion order, including success tombstones. A delayed
			// failure writer cannot resurrect a superseded delivery failure.
			if old == r || old.FinishedAt.After(r.FinishedAt) || old.FinishedAt.Equal(r.FinishedAt) && old.Delivered {
				return nil
			}
			saved.Infrastructure[i] = r
			return s.save(r.RunID, saved)
		}
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
	var failures []InfrastructureReceipt
	for _, r := range saved.Infrastructure {
		if !r.Delivered {
			failures = append(failures, r)
		}
	}
	return failures, err
}
