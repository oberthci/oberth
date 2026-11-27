package service

import (
	"context"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
)

type logRead struct {
	query  runQuery
	args   apicontract.RunLogsArgs
	action string
}

func (s *Service) PrepareRunLogs(ctx context.Context, c apicontract.Caller, q apicontract.RunLogsArgs) (apicontract.Call[apicontract.LogResult], error) {
	return s.prepareLogs(ctx, c, logRead{runQuery{ID: q.ID}, q, "run_logs"})
}
func (s *Service) PrepareLogs(ctx context.Context, c apicontract.Caller, q apicontract.LogsArgs) (apicontract.Call[apicontract.LogResult], error) {
	input := apicontract.RunLogsArgs{ByteOffset: q.ByteOffset, Container: q.Container, Containers: q.Containers, Context: q.Context, Limit: q.Limit, NodeID: q.NodeID, Offset: q.Offset, Pattern: q.Pattern, Raw: q.Raw, Step: q.Step, Tail: q.Tail}
	return s.prepareLogs(ctx, c, logRead{runQuery{SHA: q.SHA, Repo: q.Repo}, input, "logs"})
}
func (s *Service) prepareLogs(ctx context.Context, c apicontract.Caller, input logRead) (apicontract.Call[apicontract.LogResult], error) {
	a, err := s.allow(ctx, c.Actor, input.action)
	if err != nil {
		return nil, err
	}
	q := input.args
	r, err := s.resolveRun(ctx, a, input.query)
	if err != nil {
		return nil, err
	}
	q.ID = r.ID
	return bounded(func(ctx context.Context) (apicontract.LogResult, error) {
		if q.Pattern != nil && *q.Pattern != "" {
			release, err := s.reserveScan(ctx, c.CredentialID)
			if err != nil {
				return apicontract.LogResult{}, err
			}
			defer release()
		}
		return authorizedRead(ctx, s, readAuthority{a, input.action}, func(ctx context.Context, current actor.Actor) (apicontract.LogResult, error) {
			if _, err := s.resolveRepo(ctx, current, r.Repo); err != nil {
				return apicontract.LogResult{}, err
			}
			return s.readLogs(ctx, r, q)
		})
	}), nil
}

func (s *Service) readLogs(ctx context.Context, r stateport.Run, q apicontract.RunLogsArgs) (apicontract.LogResult, error) {
	purge, err := s.c.Runs.PurgeReceipt(ctx, r.ID)
	if err != nil && !isMissing(err) {
		return apicontract.LogResult{}, err
	}
	if purge.Completed {
		usage, err := s.c.Logs.Usage(ctx, r)
		return apicontract.LogResult{ID: r.ID, DisplayReason: reasonPurged, Usage: &usage}, err
	}
	out, err := s.c.Logs.Read(ctx, r, q)
	out.ID = r.ID
	return out, err
}

func (s *Service) reserveScan(ctx context.Context, credential string) (func(), error) {
	key := credential
	if credential == "" {
		return nil, refuse(forbidden)
	}
	var gate chan struct{}
	for {
		s.mu.Lock()
		var exists bool
		gate, exists = s.scans[key]
		if !exists {
			break
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-gate:
		}
	}
	gate = make(chan struct{})
	s.scans[key] = gate
	s.mu.Unlock()
	release := func() { s.mu.Lock(); delete(s.scans, key); close(gate); s.mu.Unlock() }
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}
