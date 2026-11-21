package service

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// Views supplies the HTTP reads through the same authority and log owners as tools.
func (s *Service) Views() apicontract.Views {
	return apicontract.Views{
		Counts: bindTool(s.prepareCounts), Containers: bindTool(s.prepareContainers), ContainerLog: bindTool(s.prepareContainerLog),
	}
}
func (s *Service) prepareCounts(ctx context.Context, c apicontract.Caller, _ apicontract.Empty) (apicontract.Call[apicontract.RunCounts], error) {
	a, err := s.allow(ctx, c.Actor, "run_list")
	if err != nil {
		return nil, err
	}
	scope, err := s.repositoryScope(ctx, a)
	if err != nil {
		return nil, err
	}
	return bounded(func(ctx context.Context) (apicontract.RunCounts, error) { return s.countRuns(ctx, scope) }), nil
}
func (s *Service) countRuns(ctx context.Context, scope []string) (apicontract.RunCounts, error) {
	counts, err := s.c.Runs.RunCounts(ctx, scope)
	return apicontract.RunCounts{Queued: counts[stepmodel.RunQueued], Running: counts[stepmodel.RunRunning], Passed: counts[stepmodel.RunPassed], Failed: counts[stepmodel.RunFailed], Cancelled: counts[stepmodel.RunCancelled]}, err
}

func (s *Service) prepareContainers(ctx context.Context, c apicontract.Caller, q apicontract.IDArgs) (apicontract.Call[apicontract.ContainerPage], error) {
	call, err := s.PrepareRunLogs(ctx, c, apicontract.RunLogsArgs{ID: q.ID, Containers: true})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (apicontract.ContainerPage, error) {
		result, err := call(ctx)
		containers := result.Containers
		if containers == nil {
			containers = []apicontract.Container{}
		}
		return apicontract.ContainerPage{Containers: containers}, err
	}, nil
}
func (s *Service) prepareContainerLog(ctx context.Context, c apicontract.Caller, q apicontract.ContainerArgs) (apicontract.Call[apicontract.LogResult], error) {
	return s.PrepareRunLogs(ctx, c, apicontract.RunLogsArgs{ID: q.ID, NodeID: q.NodeID, Container: q.Container, Pattern: q.Pattern, Context: q.Context, Offset: q.Offset, ByteOffset: q.ByteOffset, Limit: q.Limit, Tail: q.Tail})
}
