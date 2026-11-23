package service

import (
	"context"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

// Optional workflows never replace the newest build's promotion evidence.
// A rejected build admission remains a failed build, without a sealed file.
func (s *Service) latestBuild(ctx context.Context, repo, commit string) ([]stateport.Run, error) {
	filter := stateport.RunFilter{Repositories: []string{repo}, CommitSHA: commit, Trigger: model.TriggerBranch, Limit: stateport.RunPageLimit}
	for {
		rows, err := s.c.Runs.Runs(ctx, filter)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			filter.Before = row.Sequence
			if row.HasAdmission {
				full, err := s.c.Runs.Run(ctx, row.ID)
				if err != nil {
					return nil, err
				}
				row = full
			}
			if optionalWorkflow(row) {
				continue
			}
			return []stateport.Run{row}, nil
		}
		if len(rows) < filter.Limit {
			return nil, nil
		}
	}
}

func optionalWorkflow(r stateport.Run) bool {
	return r.Origin.RequestedFile != "" && r.Origin.RequestedFile != workflowpolicy.BuildFile ||
		r.HasAdmission && r.Admission.SelectedFile != workflowpolicy.BuildFile
}
