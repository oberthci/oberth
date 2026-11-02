package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// ResourceCoverages reads only numeric submission metadata, in one indexed read
// pool query. No workflow bodies, step logs or writer transaction are loaded.
func (s *Store) ResourceCoverages(ctx context.Context, ids []string) (map[string]model.ResourceCoverage, error) {
	out := map[string]model.ResourceCoverage{}
	if len(ids) > 512 {
		return nil, ErrInvalid
	}
	if len(ids) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	raw, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().QueryContext(ctx, `SELECT run_id,json_extract(workflow_json,'$.resources.declared'),json_extract(workflow_json,'$.resources.undeclared') FROM run_timings WHERE run_id IN (SELECT value FROM json_each(?))`, string(raw))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var declared, undeclared *int
		if err := rows.Scan(&id, &declared, &undeclared); err != nil {
			return nil, err
		}
		if declared != nil && undeclared != nil {
			c := model.ResourceCoverage{Declared: *declared, Undeclared: *undeclared}
			if c.Valid() {
				out[id] = c
			}
		}
	}
	return out, rows.Err()
}
