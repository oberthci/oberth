package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func manualIssue(t *testing.T, s *Store) model.Issue {
	t.Helper()
	issue, err := s.CreateIssue(t.Context(), stateport.Action{Actor: "operator"}, model.Issue{Kind: model.IssueManual, Title: "manual", Repo: "origin/team/repo", Upstream: "origin"})
	requireOK(t, err)
	return issue
}

func TestAtomicIssueFacts(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{
		{"issue.create", func(t *testing.T, s *Store) func(stateport.Action) error {
			return func(a stateport.Action) error {
				_, err := s.CreateIssue(t.Context(), a, model.Issue{Kind: model.IssueManual, Title: "manual"})
				return err
			}
		}},
		{"issue.update", func(t *testing.T, s *Store) func(stateport.Action) error {
			issue := manualIssue(t, s)
			return func(a stateport.Action) error {
				_, err := s.UpdateIssue(t.Context(), a, model.IssuePatch{ID: issue.ID, ExpectedRevision: &issue.Revision, Title: new("updated")})
				return err
			}
		}},
		{"issue.lock", func(t *testing.T, s *Store) func(stateport.Action) error {
			issue := manualIssue(t, s)
			return func(a stateport.Action) error {
				return s.ChangeIssueLock(t.Context(), a, stateport.IssueLockRequest{ID: issue.ID, Owner: a.Actor})
			}
		}},
		{"issue.close", func(t *testing.T, s *Store) func(stateport.Action) error {
			issue := manualIssue(t, s)
			return func(a stateport.Action) error {
				return s.CloseIssue(t.Context(), a, stateport.IssueClose{ID: issue.ID, Revision: issue.Revision})
			}
		}},
		{"issue.reopen", func(t *testing.T, s *Store) func(stateport.Action) error {
			issue := manualIssue(t, s)
			requireOK(t, s.CloseIssue(t.Context(), stateport.Action{Actor: "operator"}, stateport.IssueClose{ID: issue.ID, Revision: issue.Revision}))
			return func(a stateport.Action) error {
				return s.ReopenIssue(t.Context(), a, stateport.IssueClose{ID: issue.ID, Revision: issue.Revision + 1})
			}
		}},
		{"issue.delete", func(t *testing.T, s *Store) func(stateport.Action) error {
			issue := manualIssue(t, s)
			return func(a stateport.Action) error {
				return s.DeleteIssue(t.Context(), a, stateport.IssueClose{ID: issue.ID, Revision: issue.Revision})
			}
		}},
	})
}
