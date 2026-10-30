package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

func replayEvidenceTx(ctx context.Context, tx *sql.Tx, runID string) (model.RunReplayEvidence, error) {
	steps, err := stepResultsFrom(ctx, tx, runID)
	if err != nil {
		return model.RunReplayEvidence{}, err
	}
	var published bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM publications WHERE run_id = ?)`, runID).Scan(&published); err != nil {
		return model.RunReplayEvidence{}, fmt.Errorf("check replay publications: %w", err)
	}
	return model.RunReplayEvidence{Steps: steps, PublicationStarted: published}, nil
}
