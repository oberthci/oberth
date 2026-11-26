package service

import (
	"context"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

// Optional workflows never replace the newest build's promotion evidence.
// A rejected build admission remains a failed build, without a sealed file.
// Read at most two pages (400 runs); older evidence requires a fresh build.
func (s *Service) latestBuild(ctx context.Context, repo, commit string) ([]stateport.Run, error) {
	filter := stateport.RunFilter{Repositories: []string{repo}, CommitSHA: commit, Trigger: model.TriggerBranch, Limit: stateport.RunPageLimit}
	for range 2 {
		rows, err := s.c.Runs.Runs(ctx, filter)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			filter.Before = row.Sequence
			// A resumed run is no build of its commit: it never shadows one.
			if row.Origin.Resumed() || row.Origin.RequestedFile != "" && row.Origin.RequestedFile != workflowpolicy.BuildFile {
				continue
			}
			row, err = s.buildEvidenceRun(ctx, row)
			if err != nil {
				return nil, err
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
	return nil, publicerr.New(publicerr.Unavailable, "Build evidence scan limit reached.", "Run build.yaml again for this commit, then retry promotion.", nil)
}

func (s *Service) buildEvidenceRun(ctx context.Context, row stateport.Run) (stateport.Run, error) {
	if row.HasAdmission {
		return s.c.Runs.Run(ctx, row.ID)
	}
	return row, nil
}

func optionalWorkflow(r stateport.Run) bool {
	return r.Origin.RequestedFile != "" && r.Origin.RequestedFile != workflowpolicy.BuildFile ||
		r.HasAdmission && r.Admission.SelectedFile != workflowpolicy.BuildFile
}
