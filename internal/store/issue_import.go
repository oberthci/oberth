package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

// ImportIssues checks the entire ID set before inserts, under the writer transaction.
// Dry runs and complete replays roll back without audit, events or sequence changes.
func (s *Store) ImportIssues(ctx context.Context, a stateport.Action, q stateport.IssueImport) (stateport.IssueImportResult, error) {
	r := q.Result()
	if !a.AllowsIssueImport() {
		return r, stateport.ImportContext(refuse(forbidden, nil), 1, 0)
	}
	if err := q.Validate(); err != nil {
		return r, err
	}
	source, err := q.Source()
	if err != nil {
		return r, err
	}
	details := struct {
		SHA256 string `json:"sha256"`
		Count  int    `json:"count"`
	}{q.FileSHA256, len(q.Records)}
	err = s.change(ctx, mutation{a.Actor, "issue.import", "issue", "", details}, func(tx *sql.Tx) error {
		found, err := q.CheckReplay(source, func(id int64) (model.Issue, bool, error) {
			issue, err := readIssue(ctx, tx, id)
			if err != nil && publicerr.Classify(err).Code() == publicerr.NotFound {
				return model.Issue{}, false, nil
			}
			return issue, err == nil, err
		})
		if err != nil {
			return err
		}
		r.AlreadyImported = found
		if found {
			return unchanged{}
		}
		if err := s.insertImportedIssues(ctx, tx, q, source); err != nil {
			return err
		}

		if q.DryRun {
			return unchanged{}
		}
		return nil
	})
	return r, importOutcome(err)
}

func importOutcome(err error) error {
	if err == nil {
		return nil
	}
	var uncertain commitFailure
	if errors.As(err, &uncertain) {
		p := publicerr.Classify(err)
		p.Context = append(p.Context, "Import outcome is uncertain; run the same file again to confirm already imported.")
		return p
	}
	return stateport.ImportContext(err, 1, 0)
}

func (s *Store) insertImportedIssues(ctx context.Context, tx *sql.Tx, q stateport.IssueImport, source model.IssueImportSource) error {
	for i, v := range q.Records {
		issue := v.Issue(source)
		if err := s.saveIssue(ctx, tx, &issue, true); err != nil {
			return stateport.ImportContext(err, q.Line(i), v.ID)
		}
	}
	return nil
}
