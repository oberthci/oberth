package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// UpsertCIIssue seeds an incident without fabricating an unrelated run. Like
// production projection, it updates the body and occurrence count while preserving any investigation
// lock. Runtime projection tests
// use real run outcomes; this helper is only for unrelated API fixture setup.
func UpsertCIIssue(database *store.Store, ctx context.Context, actor string, repoID int64, branch, title, body string) (model.Issue, error) {
	if strings.TrimSpace(actor) == "" || repoID <= 0 || strings.TrimSpace(branch) == "" || strings.TrimSpace(title) == "" {
		return model.Issue{}, fmt.Errorf("%w: CI issue fixture fields", store.ErrInvalid)
	}
	db, fixture, err := fixtureDB(database)
	if err != nil {
		return model.Issue{}, err
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return model.Issue{}, err
	}
	defer tx.Rollback()
	now := fixtureNow(fixture)
	stamp := now.UnixNano()
	var id, occurrences int64
	err = tx.QueryRowContext(ctx, `INSERT INTO issues(repo_id,kind,branch,title,body,state,occurrences,created_at,updated_at)
VALUES(?,'ci',?,?,?,'open',1,?,?)
ON CONFLICT(repo_id,branch) WHERE kind='ci' AND state='open' DO UPDATE SET title=excluded.title,body=excluded.body,occurrences=issues.occurrences+1,updated_at=excluded.updated_at
RETURNING id,occurrences`, repoID, branch, title, body, stamp, stamp).Scan(&id, &occurrences)
	if err != nil {
		return model.Issue{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Issue{}, err
	}
	details, err := json.Marshal(map[string]any{"branch": branch, "occurrences": occurrences})
	if err != nil {
		return model.Issue{}, err
	}
	if _, err = database.AppendAuditAction(ctx, model.AuditActionSpec{Actor: actor, Action: "issue.ci.upsert", ResourceType: "issue", ResourceID: fmt.Sprint(id), Details: string(details)}); err != nil {
		return model.Issue{}, err
	}
	return database.Issue(ctx, id)
}
