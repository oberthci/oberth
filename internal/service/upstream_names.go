package service

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/stateport"
)

func (s *Service) registeredUpstreams(ctx context.Context) ([]string, error) {
	if s.c.People.Admin == nil {
		return nil, refuse(unavailable)
	}
	rows, err := s.c.People.Admin.UpstreamBindings(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// requireUpstreams returns the first unknown position for file diagnostics, or -1.
func (s *Service) requireUpstreams(ctx context.Context, requested []string) (int, error) {
	if len(requested) == 0 {
		return -1, nil
	}
	known, err := s.registeredUpstreams(ctx)
	if err != nil {
		return -1, err
	}
	return checkUpstreams(known, requested)
}

func checkUpstreams(known, requested []string) (int, error) {
	first := -1
	missing := []string{}
	for i, name := range requested {
		if !slices.Contains(known, name) {
			if first < 0 {
				first = i
			}
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return -1, nil
	}
	slices.Sort(missing)
	p := refuse(unknownUpstreams)
	missing = slices.Compact(missing)
	const shown = 8
	summary := quotedUpstreams(missing[:min(shown, len(missing))])
	if len(missing) > shown {
		summary += fmt.Sprintf(" and %d more", len(missing)-shown)
	}
	p.Context = append(p.Context, "Unknown upstreams: "+summary)
	return first, p
}

func quotedUpstreams(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = strconv.Quote(name)
	}
	return strings.Join(quoted, ", ")
}

// scopedUpstreamNames lists the registered upstream names the caller may see, sorted once each.
func (s *Service) scopedUpstreamNames(ctx context.Context, a actor.Actor) ([]string, error) {
	upstreams, err := s.scopedRepoUpstreams(ctx, a)
	if err != nil {
		return nil, err
	}
	choices := make([]string, 0, len(upstreams))
	for _, upstream := range upstreams {
		choices = append(choices, upstream.Name)
	}
	slices.Sort(choices)
	return slices.Compact(choices), nil
}

// upstreamRepositories lists the repositories under the requested upstreams.
// A name outside the caller's visible upstreams is refused like an unregistered one.
func (s *Service) upstreamRepositories(ctx context.Context, a actor.Actor, requested []string) ([]string, error) {
	choices, err := s.scopedUpstreamNames(ctx, a)
	if err != nil {
		return nil, err
	}
	if _, err := checkUpstreams(choices, requested); err != nil {
		return nil, err
	}
	return s.c.Catalog.RepositoryScope(ctx, requested)
}

func (s *Service) issueUpstream(ctx context.Context, a actor.Actor, requested string) (string, error) {
	choices, err := s.scopedUpstreamNames(ctx, a)
	if err != nil {
		return "", err
	}
	if requested == "" {
		switch {
		case len(choices) == 1:
			requested = choices[0]
		case !a.Admin && len(a.Scope) == 1:
			requested = a.Scope[0]
		default:
			p := refuse(issueUpstreamRequired)
			p.Context = append(p.Context, "Upstream choices: "+quotedUpstreams(choices))
			return "", p
		}
	}
	_, err = checkUpstreams(choices, []string{requested})
	return requested, err
}

func mappedIssueImport(batch stateport.IssueImport, upstream string) stateport.IssueImport {
	batch.Records = slices.Clone(batch.Records)
	for i := range batch.Records {
		record := &batch.Records[i]
		if record.Repo != "" {
			record.Repo = upstream + strings.TrimPrefix(record.Repo, record.Upstream)
		}
		record.Upstream = upstream
	}
	return batch
}
