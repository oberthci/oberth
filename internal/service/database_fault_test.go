package service

import (
	"context"
	"sync"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type deadlineFaultStore struct {
	*store.Store
	mu         sync.Mutex
	failures   map[string]int
	calls      map[string]int
	faultError error
}

func (s *deadlineFaultStore) fault(operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[operation]++
	if s.failures[operation] > 0 {
		s.failures[operation]--
		return s.faultError
	}
	return nil
}
func (s *deadlineFaultStore) count(operation string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[operation]
}

func (s *deadlineFaultStore) EnqueueReceiveEvent(a0 context.Context, a1 model.ReceiveEventSpec, a2 model.RunSpec) (model.EnqueueRunResult, error) {
	if err := s.fault("EnqueueReceiveEvent"); err != nil {
		var zero model.EnqueueRunResult
		return zero, err
	}
	return s.Store.EnqueueReceiveEvent(a0, a1, a2)
}

func (s *deadlineFaultStore) ClaimNextRun(a0 context.Context) (model.Run, error) {
	if err := s.fault("ClaimNextRun"); err != nil {
		var zero model.Run
		return zero, err
	}
	return s.Store.ClaimNextRun(a0)
}

func (s *deadlineFaultStore) MarkRunCreatePending(a0 context.Context, a1 string, a2 string) error {
	if err := s.fault("MarkRunCreatePending"); err != nil {
		return err
	}
	return s.Store.MarkRunCreatePending(a0, a1, a2)
}

func (s *deadlineFaultStore) MarkRunSubmitted(a0 context.Context, a1 string) (model.Run, error) {
	if err := s.fault("MarkRunSubmitted"); err != nil {
		var zero model.Run
		return zero, err
	}
	return s.Store.MarkRunSubmitted(a0, a1)
}

func (s *deadlineFaultStore) MarkRunAdmitted(a0 context.Context, a1 string) (model.Run, error) {
	if err := s.fault("MarkRunAdmitted"); err != nil {
		var zero model.Run
		return zero, err
	}
	return s.Store.MarkRunAdmitted(a0, a1)
}

func (s *deadlineFaultStore) MarkRunParked(a0 context.Context, a1 string) (model.Run, error) {
	if err := s.fault("MarkRunParked"); err != nil {
		var zero model.Run
		return zero, err
	}
	return s.Store.MarkRunParked(a0, a1)
}

func (s *deadlineFaultStore) Run(a0 context.Context, a1 string) (model.Run, error) {
	if err := s.fault("Run"); err != nil {
		var zero model.Run
		return zero, err
	}
	return s.Store.Run(a0, a1)
}

func (s *deadlineFaultStore) SetRunJobName(a0 context.Context, a1 string, a2 string) (model.Run, error) {
	if err := s.fault("SetRunJobName"); err != nil {
		var zero model.Run
		return zero, err
	}
	return s.Store.SetRunJobName(a0, a1, a2)
}

func (s *deadlineFaultStore) SetRunCredentialed(a0 context.Context, a1 string) error {
	if err := s.fault("SetRunCredentialed"); err != nil {
		return err
	}
	return s.Store.SetRunCredentialed(a0, a1)
}

func (s *deadlineFaultStore) SetRunConcurrencyGroup(a0 context.Context, a1 string, a2 string) error {
	if err := s.fault("SetRunConcurrencyGroup"); err != nil {
		return err
	}
	return s.Store.SetRunConcurrencyGroup(a0, a1, a2)
}

func (s *deadlineFaultStore) PendingRunCancellations(a0 context.Context) ([]model.RunCancellation, error) {
	if err := s.fault("PendingRunCancellations"); err != nil {
		var zero []model.RunCancellation
		return zero, err
	}
	return s.Store.PendingRunCancellations(a0)
}

func (s *deadlineFaultStore) CompleteRunCancellation(a0 context.Context, a1 string, a2 string) error {
	if err := s.fault("CompleteRunCancellation"); err != nil {
		return err
	}
	return s.Store.CompleteRunCancellation(a0, a1, a2)
}

func (s *deadlineFaultStore) CompleteObservedRunCancellation(a0 context.Context, a1 model.RunCancellation) error {
	if err := s.fault("CompleteObservedRunCancellation"); err != nil {
		return err
	}
	return s.Store.CompleteObservedRunCancellation(a0, a1)
}

func (s *deadlineFaultStore) RearmRunCancellation(a0 context.Context, a1 string, a2 string) error {
	if err := s.fault("RearmRunCancellation"); err != nil {
		return err
	}
	return s.Store.RearmRunCancellation(a0, a1, a2)
}

func (s *deadlineFaultStore) CompleteSupersededRunCancellationWithoutJob(a0 context.Context, a1 string) error {
	if err := s.fault("CompleteSupersededRunCancellationWithoutJob"); err != nil {
		return err
	}
	return s.Store.CompleteSupersededRunCancellationWithoutJob(a0, a1)
}

func (s *deadlineFaultStore) FinishRun(a0 context.Context, a1 string, a2 model.RunResult) (model.Run, error) {
	if err := s.fault("FinishRun"); err != nil {
		var zero model.Run
		return zero, err
	}
	return s.Store.FinishRun(a0, a1, a2)
}

func (s *deadlineFaultStore) PutStepResult(a0 context.Context, a1 model.StepResult) (model.StepResult, error) {
	if err := s.fault("PutStepResult"); err != nil {
		var zero model.StepResult
		return zero, err
	}
	return s.Store.PutStepResult(a0, a1)
}

func (s *deadlineFaultStore) RunningRunsWithJobs(a0 context.Context) ([]model.Run, error) {
	if err := s.fault("RunningRunsWithJobs"); err != nil {
		var zero []model.Run
		return zero, err
	}
	return s.Store.RunningRunsWithJobs(a0)
}

func (s *deadlineFaultStore) RequeueStrandedRun(a0 context.Context, a1 string) (model.Run, error) {
	if err := s.fault("RequeueStrandedRun"); err != nil {
		var zero model.Run
		return zero, err
	}
	return s.Store.RequeueStrandedRun(a0, a1)
}

func (s *deadlineFaultStore) BeginPublication(a0 context.Context, a1 model.PublicationSpec) (model.Publication, error) {
	if err := s.fault("BeginPublication"); err != nil {
		var zero model.Publication
		return zero, err
	}
	return s.Store.BeginPublication(a0, a1)
}

func (s *deadlineFaultStore) SetPublicationPredecessor(a0 context.Context, a1 string, a2 string, a3 bool) (model.Publication, error) {
	if err := s.fault("SetPublicationPredecessor"); err != nil {
		var zero model.Publication
		return zero, err
	}
	return s.Store.SetPublicationPredecessor(a0, a1, a2, a3)
}

func (s *deadlineFaultStore) Publication(a0 context.Context, a1 string) (model.Publication, error) {
	if err := s.fault("Publication"); err != nil {
		var zero model.Publication
		return zero, err
	}
	return s.Store.Publication(a0, a1)
}

func (s *deadlineFaultStore) PendingPublications(a0 context.Context) ([]model.Publication, error) {
	if err := s.fault("PendingPublications"); err != nil {
		var zero []model.Publication
		return zero, err
	}
	return s.Store.PendingPublications(a0)
}

func (s *deadlineFaultStore) FinalizePublication(a0 context.Context, a1 string, a2 model.PublicationStatus, a3 string) (model.PublicationFinalization, error) {
	if err := s.fault("FinalizePublication"); err != nil {
		var zero model.PublicationFinalization
		return zero, err
	}
	return s.Store.FinalizePublication(a0, a1, a2, a3)
}

func (s *deadlineFaultStore) RetryFailedPublication(a0 context.Context, a1 string, a2 string, a3 string) (model.Publication, error) {
	if err := s.fault("RetryFailedPublication"); err != nil {
		var zero model.Publication
		return zero, err
	}
	return s.Store.RetryFailedPublication(a0, a1, a2, a3)
}

func (s *deadlineFaultStore) RunWorkspaceCleanupEligible(a0 context.Context, a1 string) (bool, error) {
	if err := s.fault("RunWorkspaceCleanupEligible"); err != nil {
		var zero bool
		return zero, err
	}
	return s.Store.RunWorkspaceCleanupEligible(a0, a1)
}

func (s *deadlineFaultStore) PromotionWorkspaceCleanupEligible(a0 context.Context, a1 string) (bool, error) {
	if err := s.fault("PromotionWorkspaceCleanupEligible"); err != nil {
		var zero bool
		return zero, err
	}
	return s.Store.PromotionWorkspaceCleanupEligible(a0, a1)
}

func (s *deadlineFaultStore) RunWorkspaceCleanupCandidates(a0 context.Context, a1 string, a2 int) ([]string, error) {
	if err := s.fault("RunWorkspaceCleanupCandidates"); err != nil {
		var zero []string
		return zero, err
	}
	return s.Store.RunWorkspaceCleanupCandidates(a0, a1, a2)
}

func (s *deadlineFaultStore) PromotionWorkspaceCleanupCandidates(a0 context.Context, a1 string, a2 int) ([]string, error) {
	if err := s.fault("PromotionWorkspaceCleanupCandidates"); err != nil {
		var zero []string
		return zero, err
	}
	return s.Store.PromotionWorkspaceCleanupCandidates(a0, a1, a2)
}

func (s *deadlineFaultStore) Repository(a0 context.Context, a1 int64) (model.Repository, error) {
	if err := s.fault("Repository"); err != nil {
		var zero model.Repository
		return zero, err
	}
	return s.Store.Repository(a0, a1)
}

func (s *deadlineFaultStore) Upstream(a0 context.Context, a1 int64) (model.Upstream, error) {
	if err := s.fault("Upstream"); err != nil {
		var zero model.Upstream
		return zero, err
	}
	return s.Store.Upstream(a0, a1)
}

func (s *deadlineFaultStore) PromotionByRun(a0 context.Context, a1 string) (model.Promotion, error) {
	if err := s.fault("PromotionByRun"); err != nil {
		var zero model.Promotion
		return zero, err
	}
	return s.Store.PromotionByRun(a0, a1)
}

func (s *deadlineFaultStore) Promotion(a0 context.Context, a1 string) (model.Promotion, error) {
	if err := s.fault("Promotion"); err != nil {
		var zero model.Promotion
		return zero, err
	}
	return s.Store.Promotion(a0, a1)
}
