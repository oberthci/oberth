package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func (s *Store) RecordRunSources(ctx context.Context, id, sha string, sources []pipeline.PinnedSource) error {
	if !validOID(sha) || len(sources) == 0 {
		return fmt.Errorf("%w: source binding", ErrInvalid)
	}
	if err := argoworkflow.ValidatePinnedSources(sources); err != nil {
		return fmt.Errorf("%w: source binding: %v", ErrInvalid, err)
	}
	body, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET source_bindings=? WHERE id=? AND (CASE WHEN tested_sha<>'' THEN tested_sha ELSE sha END)=? AND source_bindings='' AND status IN ('queued','running')`, string(body), id, sha)
	if err != nil {
		return fmt.Errorf("record run sources: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var existing string
		if err := s.reader().QueryRowContext(ctx, `SELECT source_bindings FROM runs WHERE id=? AND (CASE WHEN tested_sha<>'' THEN tested_sha ELSE sha END)=?`, id, sha).Scan(&existing); err != nil {
			return fmt.Errorf("read run sources: %w", err)
		}
		if existing != string(body) {
			return fmt.Errorf("%w: source binding already recorded or run terminal", ErrInvalidState)
		}
	}
	return nil
}
