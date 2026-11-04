package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func greenTrailerPublication(t *testing.T, s *Store, repo model.Repository, ref, trigger, sha string) model.PublicationFinalization {
	t.Helper()
	ctx := t.Context()
	run, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: ref, SHA: sha, Actor: "owner", Trigger: trigger})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	pub, err := s.BeginPublication(ctx, model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: sha, Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	final, err := s.FinalizePublication(ctx, pub.ID, model.PublicationDelivered, "")
	if err != nil {
		t.Fatal(err)
	}
	return final
}

func TestIssueTrailerEvidenceScopeLockAndReplay(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := t.Context()
	other, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "other", Kind: "ssh", BaseURL: "ssh://example.invalid/team"})
	if err != nil {
		t.Fatal(err)
	}
	makeIssue := func(scope int64) model.Issue {
		i, e := s.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: "repair", ScopeUpstreamID: scope})
		if e != nil {
			t.Fatal(e)
		}
		return i
	}
	closeIssue, ref, locked, owned, closed, hidden, global := makeIssue(repo.UpstreamID), makeIssue(repo.UpstreamID), makeIssue(repo.UpstreamID), makeIssue(repo.UpstreamID), makeIssue(repo.UpstreamID), makeIssue(other.ID), makeIssue(0)
	for _, entry := range []struct {
		id    int64
		owner string
	}{{locked.ID, "owner"}, {owned.ID, "oberth"}} {
		if _, err = s.AcquireIssueLock(ctx, entry.id, entry.owner); err != nil {
			t.Fatal(err)
		}
	}
	state := model.IssueClosed
	if _, err = s.UpdateManualIssue(ctx, "owner", closed.ID, model.IssuePatch{State: &state}); err != nil {
		t.Fatal(err)
	}
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	final := greenTrailerPublication(t, s, repo, "main", "push", sha)
	pending, err := s.PendingIssueTrailerWork(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%#v %v", pending, err)
	}
	changes := []model.CommitIssueTrailer{{IssueID: closeIssue.ID, SHA: sha, Close: true}, {IssueID: ref.ID, SHA: sha}, {IssueID: ref.ID, SHA: sha}}
	for _, i := range []model.Issue{locked, owned, closed, hidden, global} {
		changes = append(changes, model.CommitIssueTrailer{IssueID: i.ID, SHA: sha, Close: true})
	}
	changes = append(changes, model.CommitIssueTrailer{IssueID: 99999, SHA: sha, Close: true})
	for range 2 {
		if err = s.CompleteIssueTrailerWork(ctx, final.Publication.ID, changes); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		i     model.Issue
		want  model.IssueState
		lines int
	}{{closeIssue, model.IssueClosed, 1}, {ref, model.IssueOpen, 1}, {locked, model.IssueOpen, 0}, {owned, model.IssueOpen, 0}, {closed, model.IssueClosed, 0}, {hidden, model.IssueOpen, 0}, {global, model.IssueOpen, 0}} {
		got, e := s.Issue(ctx, tt.i.ID)
		if e != nil {
			t.Fatal(e)
		}
		if got.State != tt.want || strings.Count(got.Body, final.Run.ID) != tt.lines {
			t.Fatalf("issue=%#v want state=%s lines=%d", got, tt.want, tt.lines)
		}
	}
	for _, action := range []string{"issue.append", "issue.close", "issue.trailer.locked"} {
		var count int
		if err = s.db.QueryRow(`SELECT count(*) FROM audit_actions WHERE actor='oberth' AND action=? AND json_extract(details,'$.run_id')=?`, action, final.Run.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 2
		if action == "issue.close" {
			want = 1
		}
		if count != want {
			t.Fatalf("%s count=%d want=%d", action, count, want)
		}
	}
	if _, err = s.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIssueTrailerRangeAndAuditRollback(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	first := greenTrailerPublication(t, s, repo, "main", "push", strings.Repeat("a", 40))
	if err := s.CompleteIssueTrailerWork(ctx, first.Publication.ID, nil); err != nil {
		t.Fatal(err)
	}
	greenTrailerPublication(t, s, repo, "feature", "push", strings.Repeat("b", 40))
	greenTrailerPublication(t, s, repo, "main", "schedule", strings.Repeat("c", 40))
	second := greenTrailerPublication(t, s, repo, "main", "push", strings.Repeat("d", 40))
	pending, err := s.PendingIssueTrailerWork(ctx)
	if err != nil || len(pending) != 1 || pending[0].PreviousSHA != first.Publication.ResultSHA {
		t.Fatalf("range=%#v %v", pending, err)
	}
	issue, err := s.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: "repair", ScopeUpstreamID: repo.UpstreamID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER reject_trailer_close BEFORE INSERT ON audit_actions WHEN NEW.action='issue.close' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	changes := []model.CommitIssueTrailer{{IssueID: issue.ID, SHA: second.Publication.ResultSHA, Close: true}}
	if err = s.CompleteIssueTrailerWork(ctx, second.Publication.ID, changes); err == nil {
		t.Fatal("audit failure accepted")
	}
	got, err := s.Issue(ctx, issue.ID)
	if err != nil || got.State != model.IssueOpen || got.Body != "" {
		t.Fatalf("rollback=%#v %v", got, err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER reject_trailer_close`); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteIssueTrailerWork(ctx, second.Publication.ID, changes); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingIssueTrailerWork(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("completed=%#v %v", pending, err)
	}
	var n int
	if err = s.db.QueryRow(`SELECT count(*) FROM audit_actions WHERE resource_type='issue' AND resource_id=? AND actor='oberth'`, fmt.Sprint(issue.ID)).Scan(&n); err != nil || n != 2 {
		t.Fatalf("audit count=%d %v", n, err)
	}
}

func TestIssueTrailerMigrationSeedsHistoricalBoundary(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := t.Context()
	first := greenTrailerPublication(t, s, repo, "main", "branch", strings.Repeat("a", 40))
	// Recreate only the new migration against a schema-54 database with history.
	if _, err := s.db.Exec(`DROP TABLE issue_trailer_publications; DELETE FROM schema_migrations WHERE version=55;`); err != nil {
		t.Fatal(err)
	}
	path := s.livePath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pending, err := s.PendingIssueTrailerWork(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("historical replay=%#v %v", pending, err)
	}
	greenTrailerPublication(t, s, repo, "main", "branch", strings.Repeat("b", 40))
	pending, err = s.PendingIssueTrailerWork(ctx)
	if err != nil || len(pending) != 1 || pending[0].PreviousSHA != first.Publication.ResultSHA {
		t.Fatalf("seeded boundary=%#v %v", pending, err)
	}
}

func TestIssueTrailerNonGreenAndTagDoNothing(t *testing.T) {
	for _, tt := range []struct {
		name   string
		kind   model.RefKind
		status model.PublicationStatus
	}{{"failed", model.RefBranch, model.PublicationFailed}, {"abandoned", model.RefBranch, model.PublicationAbandoned}, {"tag", model.RefTag, model.PublicationDelivered}} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			ctx := t.Context()
			run, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: tt.kind, Ref: "main", SHA: strings.Repeat("a", 40), Actor: "owner", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ClaimNextRun(ctx); err != nil {
				t.Fatal(err)
			}
			pub, err := s.BeginPublication(ctx, model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: tt.kind, Ref: "main", ResultSHA: run.SHA, Actor: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			if tt.status == model.PublicationAbandoned {
				_, err = s.AbandonPublication(ctx, pub.ID, "owner", "cancel publication")
			} else {
				_, err = s.FinalizePublication(ctx, pub.ID, tt.status, "upstream rejected")
			}
			if err != nil {
				t.Fatal(err)
			}
			pending, err := s.PendingIssueTrailerWork(ctx)
			if err != nil || len(pending) != 0 {
				t.Fatalf("ineligible work=%#v %v", pending, err)
			}
		})
	}
}

func TestIssueTrailerPreservesEveryReferencedCommit(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := t.Context()
	issue, err := s.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: "repair", ScopeUpstreamID: repo.UpstreamID})
	if err != nil {
		t.Fatal(err)
	}
	final := greenTrailerPublication(t, s, repo, "main", "branch", strings.Repeat("d", 40))
	changes := []model.CommitIssueTrailer{{IssueID: issue.ID, SHA: strings.Repeat("a", 40)}, {IssueID: issue.ID, SHA: strings.Repeat("b", 40)}, {IssueID: issue.ID, SHA: strings.Repeat("c", 40), Close: true}, {IssueID: issue.ID, SHA: strings.Repeat("d", 40)}}
	for range 2 {
		if err = s.CompleteIssueTrailerWork(ctx, final.Publication.ID, changes); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Issue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.IssueClosed || strings.Count(got.Body, final.Run.ID) != 3 || !strings.Contains(got.Body, strings.Repeat("a", 12)) || !strings.Contains(got.Body, strings.Repeat("b", 12)) || !strings.Contains(got.Body, strings.Repeat("c", 12)) || strings.Contains(got.Body, strings.Repeat("d", 12)) {
		t.Fatalf("commit evidence=%#v", got)
	}
}

func TestIssueTrailerPendingDoesNotLetOneRepositoryFillBatch(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	healthy, err := s.CreateRepository(t.Context(), model.RepositorySpec{Name: "healthy", UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	// Leave 100 older rows pending, as with unavailable Git or an invalid policy.
	for i := range 100 {
		greenTrailerPublication(t, s, repo, "main", "push", fmt.Sprintf("%040x", i+1))
	}
	final := greenTrailerPublication(t, s, healthy, "main", "push", strings.Repeat("b", 40))
	pending, err := s.PendingIssueTrailerWork(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 100 {
		t.Fatalf("batch size=%d, want 100", len(pending))
	}
	for _, w := range pending {
		if w.PublicationID == final.Publication.ID {
			return
		}
	}
	t.Fatal("100 pending rows from one repository starved the healthy repository")
}

func TestIssueTrailerSkipIsAuditedAtomicAndIdempotent(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	ctx := t.Context()
	repo := createRepo(t, s)
	issue, err := s.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: "repair", Body: "original", ScopeUpstreamID: repo.UpstreamID})
	if err != nil {
		t.Fatal(err)
	}
	final := greenTrailerPublication(t, s, repo, "main", "push", strings.Repeat("a", 40))
	reason := "issue trailer input rejected: invalid issue trailer policy"
	for _, invalid := range []string{"", " ", strings.Repeat("x", 1025), "bad\x00reason"} {
		if err = s.SkipIssueTrailerWork(ctx, final.Publication.ID, invalid); err == nil {
			t.Fatalf("accepted skip reason %q", invalid)
		}
	}
	if _, err = s.db.Exec(`CREATE TRIGGER reject_trailer_skip BEFORE INSERT ON audit_actions WHEN NEW.action='issue.trailer.skipped' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.SkipIssueTrailerWork(ctx, final.Publication.ID, reason); err == nil {
		t.Fatal("accepted missing skip audit")
	}
	pending, err := s.PendingIssueTrailerWork(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("audit failure lost work: %d %v", len(pending), err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER reject_trailer_skip`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.SkipIssueTrailerWork(ctx, final.Publication.ID, reason); err != nil {
			t.Fatal(err)
		}
	}
	// Completed rejection cannot later be replayed as evidence or closure.
	if err = s.CompleteIssueTrailerWork(ctx, final.Publication.ID, []model.CommitIssueTrailer{{IssueID: issue.ID, SHA: final.Publication.ResultSHA, Close: true}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Issue(ctx, issue.ID)
	if err != nil || got.State != model.IssueOpen || got.Body != "original" {
		t.Fatalf("skip mutated issue: %#v %v", got, err)
	}
	actions, _, err := s.ListAuditActions(ctx, model.AuditListFilter{ActionPrefix: "issue.trailer.skipped"})
	if err != nil || len(actions) != 1 {
		t.Fatalf("skip audit count=%d %v", len(actions), err)
	}
	a := actions[0]
	var details map[string]string
	if err = json.Unmarshal([]byte(a.Details), &details); err != nil {
		t.Fatal(err)
	}
	if a.Actor != "oberth" || a.ResourceType != "publication" || a.ResourceID != final.Publication.ID || details["reason"] != reason || details["run_id"] != final.Run.ID || details["sha"] != final.Publication.ResultSHA || details["branch"] != "main" || details["publication_id"] != final.Publication.ID || details["repo"] == "" {
		t.Fatalf("audit=%#v details=%#v", a, details)
	}
	pending, err = s.PendingIssueTrailerWork(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("skip still pending: %d %v", len(pending), err)
	}
	if _, err = s.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
}
