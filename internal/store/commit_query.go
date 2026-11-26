package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/oberthci/oberth/internal/stateport"
)

// RunByCommit resolves only visible object/commit prefixes, newest run per commit;
// a resumed run is never selected by commit.
func (s *Store) RunByCommit(ctx context.Context, repositories []string, prefix string) (stateport.Run, error) {
	if !stateport.ValidCommitPrefix(prefix) {
		return stateport.Run{}, refuse(invalidCommit, nil)
	}
	prefix = strings.ToLower(prefix)
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return stateport.Run{}, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	query, args := commitQuery(repositories, prefix)
	ids, err := unboundIDs(ctx, tx, query, args)
	if err != nil {
		return stateport.Run{}, err
	}
	if len(ids) == 0 {
		return stateport.Run{}, refuse(missing, nil)
	}
	if len(ids) > 1 {
		return stateport.Run{}, refuse(ambiguousCommit, nil)
	}
	return loadRun(ctx, tx, ids[0])
}

func commitQuery(repositories []string, prefix string) (string, []any) {
	predicate, scopeArgs := scopePredicate("p.canonical", repositories)
	parts := make([]string, 0, 2)
	args := make([]any, 0)
	for _, field := range []string{"commit_sha", "object_sha"} {
		index := "run_commit"
		if field == "object_sha" {
			index = "run_object"
		}
		column := `json_extract(r.summary,'$.` + field + `')`
		parts = append(parts, `SELECT r.sequence,coalesce(json_extract(r.summary,'$.commit_sha'),json_extract(r.summary,'$.object_sha')) AS identity
FROM runs r INDEXED BY `+index+` CROSS JOIN repositories p ON p.id=r.repo_id WHERE `+predicate+` AND `+column+`>=? AND `+column+`<? AND `+unresumed("r"))
		args = append(args, scopeArgs...)
		args = append(args, prefix, prefixEnd(prefix))
	}
	query := `WITH candidates AS(` + strings.Join(parts, ` UNION `) + `), newest AS(
SELECT max(sequence) AS sequence FROM candidates GROUP BY identity ORDER BY max(sequence) DESC LIMIT 2)
SELECT id FROM runs JOIN newest ON newest.sequence=runs.sequence ORDER BY runs.sequence DESC`
	return query, args
}
