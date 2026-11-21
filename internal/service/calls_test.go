package service_test

import (
	"context"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// testService invokes exact transport handlers; query builders live only in tests.
type testService struct{ *service.Service }

func execute[T any](ctx context.Context, call apicontract.Call[T], err error) (T, error) {
	if err != nil {
		var zero T
		return zero, err
	}
	return call(ctx)
}
func (s *testService) RunGet(ctx context.Context, a actor.Actor, q viewQuery) (apicontract.RunRecord, error) {
	call, err := s.PrepareRunGet(ctx, apicontract.Caller{Actor: a}, apicontract.RunGetArgs{ID: q.ID, StepOffset: q.Offset, StepLimit: q.Limit, View: func() string {
		if q.Helpers {
			return "full"
		}
		return "summary"
	}()})
	return execute(ctx, call, err)
}
func (s *testService) RunList(ctx context.Context, a actor.Actor, q viewQuery) (apicontract.RunPage, error) {
	call, err := s.PrepareRunList(ctx, a, q)
	return execute(ctx, call, err)
}
func (s *testService) PrepareRunList(ctx context.Context, a actor.Actor, q viewQuery) (apicontract.Call[apicontract.RunPage], error) {
	return s.Service.PrepareRunList(ctx, apicontract.Caller{Actor: a}, apicontract.RunListArgs{Repo: q.Repo, Before: q.Before, Ref: q.Ref, RefGlob: q.RefGlob, Status: string(q.Status), Latest: q.Latest, Own: q.Own, Held: q.Held, Limit: q.Limit})
}
func (s *testService) Status(ctx context.Context, a actor.Actor, q viewQuery) (stepmodel.RunSummary, error) {
	if q.ID != "" && q.Repo == "" {
		run, err := s.RunGet(ctx, a, q)
		return run.RunSummary, err
	}
	ref := q.Ref
	if q.SHA != "" {
		ref = q.SHA
	}
	if q.ID != "" {
		ref = q.ID
	}
	call, err := s.PrepareStatus(ctx, apicontract.Caller{Actor: a}, apicontract.StatusArgs{Repo: q.Repo, Ref: ref})
	return execute(ctx, call, err)
}
func (s *testService) Repos(ctx context.Context, a actor.Actor) ([]apicontract.Repository, error) {
	call, err := s.PrepareRepoList(ctx, apicontract.Caller{Actor: a}, apicontract.RepoListArgs{})
	out, err := execute(ctx, call, err)
	return out.Repositories, err
}
func (s *testService) Cancel(ctx context.Context, a actor.Actor, q actionInput) (apicontract.Receipt, error) {
	call, err := s.PrepareCancel(ctx, a, q)
	return execute(ctx, call, err)
}
func (s *testService) PrepareCancel(ctx context.Context, a actor.Actor, q actionInput) (apicontract.Call[apicontract.Receipt], error) {
	return s.PrepareRunCancel(ctx, apicontract.Caller{Actor: a}, apicontract.RunCancelArgs{ID: q.ID, Force: q.Force, Reason: q.Reason})
}
func (s *testService) Rerun(ctx context.Context, a actor.Actor, q actionInput) (apicontract.Receipt, error) {
	call, err := s.PrepareRunRerun(ctx, apicontract.Caller{Actor: a}, apicontract.RunRerunArgs{ID: q.ID, Assist: q.Assist, Reason: q.Reason})
	return execute(ctx, call, err)
}
func (s *testService) Purge(ctx context.Context, a actor.Actor, q actionInput) (apicontract.Receipt, error) {
	call, err := s.PrepareRunPurge(ctx, apicontract.Caller{Actor: a}, apicontract.RunPurgeArgs{ID: q.ID, What: q.What, Reason: q.Reason})
	return execute(ctx, call, err)
}
func (s *testService) Check(ctx context.Context, a actor.Actor, q service.CheckRequest) (apicontract.RunCheckResult, error) {
	call, err := s.PrepareRunCheck(ctx, apicontract.Caller{Actor: a}, apicontract.RunCheckArgs{Repo: q.Request.Repo, Ref: q.Request.Ref, Files: q.Files})
	return execute(ctx, call, err)
}
func seconds(d time.Duration) int {
	if d < 0 {
		return -1
	}
	return int((d + time.Second - 1) / time.Second)
}
func (s *testService) PrepareWait(ctx context.Context, a actor.Actor, q viewQuery, d time.Duration) (apicontract.Call[apicontract.WaitResult], error) {
	if q.ID != "" {
		return s.PrepareRunWait(ctx, apicontract.Caller{Actor: a}, apicontract.RunWaitArgs{ID: q.ID, Until: q.Until, TimeoutS: seconds(d)})
	}
	return s.Service.PrepareWait(ctx, apicontract.Caller{Actor: a}, apicontract.WaitArgs{Repo: q.Repo, Ref: q.Ref, SHA: q.SHA, TimeoutS: seconds(d)})
}
func (s *testService) RunWait(ctx context.Context, a actor.Actor, q viewQuery, d time.Duration) (apicontract.WaitResult, error) {
	call, err := s.PrepareWait(ctx, a, q, d)
	return execute(ctx, call, err)
}
func (s *testService) Wait(ctx context.Context, a actor.Actor, q viewQuery, d time.Duration) (apicontract.WaitResult, error) {
	return s.RunWait(ctx, a, q, d)
}
func sinceArgs(q eventsInput) apicontract.EventsWaitArgs {
	kinds := []string{}
	for _, k := range q.Kinds {
		kinds = append(kinds, string(k))
	}
	return apicontract.EventsWaitArgs{AfterID: &q.After, Repos: q.Repos, RefsGlob: q.RefsGlob, Kinds: kinds, Limit: q.Limit, Own: q.Own}
}
func (s *testService) EventsWait(ctx context.Context, a actor.Actor, q eventsInput) (apicontract.EventResult, error) {
	base := sinceArgs(q)
	call, err := s.PrepareEventsWait(ctx, apicontract.Caller{Actor: a}, apicontract.EventsWaitArgs{AfterID: base.AfterID, Repos: base.Repos, RefsGlob: base.RefsGlob, Kinds: base.Kinds, Limit: q.Limit, Own: q.Own, RunIDs: q.RunIDs, TimeoutS: seconds(q.Timeout)})
	return execute(ctx, call, err)
}
func (s *testService) EventsSince(ctx context.Context, a actor.Actor, q eventsInput) (apicontract.EventResult, error) {
	if len(q.RunIDs) > 0 {
		return s.EventsWait(ctx, a, q)
	}
	return service.ReadEventSnapshotForTest(ctx, s.Service, apicontract.Caller{Actor: a}, sinceArgs(q))
}
func (s *testService) SyncStatus(ctx context.Context, a actor.Actor) (apicontract.SyncStatus, error) {
	call, err := s.PrepareSyncStatus(ctx, apicontract.Caller{Actor: a}, apicontract.SyncStatusArgs{})
	return execute(ctx, call, err)
}

type logRequest struct {
	viewQuery
	Step                   string
	StepSet                bool
	Context, Offset, Limit *int
	Tail                   *bool
	ByteOffset             *int64
	Selector               stepmodel.LogSelector
	Pattern                string
	Containers, Raw        bool
	CredentialID           string
}

func (s *testService) PrepareLogs(ctx context.Context, a actor.Actor, q logRequest) (apicontract.Call[apicontract.LogResult], error) {
	c := apicontract.Caller{Actor: a, CredentialID: q.CredentialID}
	step := q.Step
	if step == "" && q.CredentialID != "" {
		step = q.CredentialID
	}
	args := apicontract.RunLogsArgs{ID: q.ID, Step: step, Context: q.Context, Offset: q.Offset, Limit: q.Limit, Tail: q.Tail, ByteOffset: q.ByteOffset, NodeID: q.Selector.NodeID, Container: q.Selector.Container, Containers: q.Containers, Raw: q.Raw}
	if q.Pattern != "" {
		args.Pattern = &q.Pattern
	}
	if q.ID != "" {
		return s.PrepareRunLogs(ctx, c, args)
	}
	sha := q.SHA
	if sha == "" {
		sha = q.Ref
	}
	return s.Service.PrepareLogs(ctx, c, apicontract.LogsArgs{SHA: sha, Repo: q.Repo, Step: step, Context: q.Context, Offset: q.Offset, Limit: q.Limit, Tail: q.Tail, ByteOffset: q.ByteOffset, NodeID: q.Selector.NodeID, Container: q.Selector.Container, Containers: q.Containers, Raw: q.Raw, Pattern: args.Pattern})
}
func (s *testService) RunLogs(ctx context.Context, a actor.Actor, q logRequest) (apicontract.LogResult, error) {
	call, err := s.PrepareLogs(ctx, a, q)
	return execute(ctx, call, err)
}
func (s *testService) Logs(ctx context.Context, a actor.Actor, q logRequest) (apicontract.LogResult, error) {
	return s.RunLogs(ctx, a, q)
}

type viewQuery struct {
	Repo, ID, SHA, Ref string
	Offset, Limit      int
	Helpers            bool
	Before, RefGlob    string
	Status             stepmodel.RunStatus
	Latest, Own, Held  bool
	Until              string
}
type actionInput struct {
	ID, Reason, What string
	Force, Assist    bool
}
type eventsInput struct {
	After                   int64
	Limit                   int
	Repos, RunIDs, RefsGlob []string
	Kinds                   []model.EventKind
	Own                     bool
	Timeout                 time.Duration
}
