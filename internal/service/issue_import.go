package service

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/stateport"
)

func (s *Service) PrepareIssueImport(ctx context.Context, c apicontract.Caller, q apicontract.IssueImportArgs) (apicontract.Call[stateport.IssueImportResult], error) {
	a, err := s.peopleAllow(ctx, c.Actor, "issue_import")
	if err != nil {
		return nil, stateport.ImportContext(err, 1, 0)
	}
	batch, err := DecodeIssueImport(q)
	if err != nil {
		return nil, err
	}
	return bounded(func(ctx context.Context) (stateport.IssueImportResult, error) {
		live, err := s.peopleAllow(ctx, a, "issue_import")
		if err != nil {
			return stateport.IssueImportResult{}, stateport.ImportContext(err, 1, 0)
		}
		current := batch
		names, err := importUpstreamNames(batch, q.Upstream)
		if err != nil {
			return stateport.IssueImportResult{}, err
		}
		if q.Upstream != "" {
			current = mappedIssueImport(batch, q.Upstream)
		}
		if first, err := s.requireUpstreams(ctx, names); err != nil {
			line, id := 1, int64(0)
			if q.Upstream == "" && first >= 0 {
				line, id = batch.Line(first), batch.Records[first].ID
			}
			return stateport.IssueImportResult{}, stateport.ImportContext(err, line, id)
		}
		return s.c.People.Issues.ImportIssues(ctx, stateport.Action{Actor: live.Identity, Caller: live.Clone()}, current)
	}), nil
}

func importUpstreamNames(batch stateport.IssueImport, target string) ([]string, error) {
	if target != "" {
		if reponame.ValidateUpstreamName(target) != nil {
			return nil, stateport.ImportProblem(stateport.ImportScope, 1, 0, nil)
		}
		return []string{target}, nil
	}
	names := make([]string, 0, len(batch.Records))
	for i, record := range batch.Records {
		if reponame.ValidateUpstreamName(record.Upstream) != nil {
			return nil, stateport.ImportProblem(stateport.ImportScope, batch.Line(i), record.ID, nil)
		}
		names = append(names, record.Upstream)
	}
	return names, nil
}
