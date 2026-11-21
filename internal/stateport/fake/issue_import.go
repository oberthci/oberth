package fake

import (
	"context"
	"encoding/json"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func (f *Runs) ImportIssues(ctx context.Context, a stateport.Action, q stateport.IssueImport) (stateport.IssueImportResult, error) {
	r := q.Result()
	if !a.AllowsIssueImport() {
		return r, stateport.ImportContext(runProblem(publicerr.Forbidden), 1, 0)
	}
	if err := q.Validate(); err != nil {
		return r, err
	}
	source, err := q.Source()
	if err != nil {
		return r, err
	}
	err = f.movingChange(ctx, "ImportIssues", nil, func(m *runMemory) error {
		found, err := q.CheckReplay(source, func(id int64) (model.Issue, bool, error) {
			issue, ok := m.issues[id]
			return issue, ok, nil
		})
		if err != nil {
			return err
		}
		r.AlreadyImported = found
		if found {
			return noRunChange{}
		}
		for i, v := range q.Records {
			if err := m.saveIssue(v.Issue(source)); err != nil {
				return stateport.ImportContext(err, q.Line(i), v.ID)
			}
			m.issueID = max(m.issueID, v.ID)
		}
		if q.DryRun {
			return noRunChange{}
		}
		details, _ := json.Marshal(struct {
			SHA256 string `json:"sha256"`
			Count  int    `json:"count"`
		}{q.FileSHA256, len(q.Records)})
		m.appendAudit(model.AuditAction{Actor: a.Actor, Action: "issue.import", ResourceType: "issue", Details: string(details), CreatedAt: m.now})
		return nil
	})
	return r, stateport.ImportContext(err, 1, 0)
}
