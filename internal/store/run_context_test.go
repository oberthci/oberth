package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRunRepositoryContextSupersededRule(t *testing.T) {
	for _, tc := range []struct {
		name, status                            string
		otherRepo, otherRef, otherKind, reverse bool
		want                                    bool
	}{
		{name: "newer queued", status: "queued", want: true},
		{name: "newer running", status: "running", want: true},
		{name: "same SHA still newer", status: "queued", want: true},
		{name: "newer passed", status: "passed"},
		{name: "newer failed", status: "failed"},
		{name: "newer interrupted", status: "interrupted"},
		{name: "different repository", status: "queued", otherRepo: true},
		{name: "different ref", status: "queued", otherRef: true},
		{name: "branch and tag are distinct", status: "queued", otherKind: true},
		{name: "older active does not supersede", status: "queued", reverse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			spec := testRunSpec(repo.ID, "feature/context", strings.Repeat("a", 40))
			spec.Trigger = "promotion" // Independent attempts avoid enqueue-time cancellation.
			first, err := s.EnqueueRun(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name != "same SHA still newer" {
				spec.SHA = strings.Repeat("b", 40)
			}
			if tc.otherRepo {
				other, err := s.CreateRepository(t.Context(), model.RepositorySpec{Name: "other", UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
				if err != nil {
					t.Fatal(err)
				}
				spec.RepoID = other.ID
			}
			if tc.otherRef {
				spec.Ref = "feature/other"
			}
			if tc.otherKind {
				spec.RefKind = model.RefTag
			}
			second, err := s.EnqueueRun(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(t.Context(), `UPDATE runs SET status=? WHERE id=?`, tc.status, second.ID); err != nil {
				t.Fatal(err)
			}
			target := first.Run
			if tc.reverse {
				target = second.Run
			}
			run, err := s.ResolveRunID(t.Context(), target.ID)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(run)
			var fields map[string]any
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["Repo"] != repo.Name || fields["superseded"] != tc.want {
				t.Fatalf("run context = %s, want repo %q superseded %t", body, repo.Name, tc.want)
			}
		})
	}
}

func TestRunRepositoryContextFilterBeforeLimit(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	spec := testRunSpec(repo.ID, "feature/context", strings.Repeat("a", 40))
	spec.Trigger = "promotion"
	old, err := s.EnqueueRun(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	newest, err := s.EnqueueRun(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	// A newer nonmatching history row must not consume the one-row page.
	spec.Ref = "feature/noise"
	noise, err := s.EnqueueRun(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `UPDATE runs SET status='passed', finished_at=? WHERE id=?`, unixNano(now), noise.ID); err != nil {
		t.Fatal(err)
	}
	// Newest active work is outside history and outside its page, but must still count.
	if _, err := s.db.ExecContext(t.Context(), `UPDATE runs SET status='failed', finished_at=? WHERE id=?`, unixNano(now), old.ID); err != nil {
		t.Fatal(err)
	}
	var filter model.RunListFilter
	if err := json.Unmarshal([]byte(`{"Superseded":true,"Terminal":true,"Limit":1}`), &filter); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListRecentRuns(t.Context(), filter)
	if err != nil || len(rows) != 1 || rows[0].ID != old.ID {
		t.Fatalf("history=%+v %v", rows, err)
	}
	body, _ := json.Marshal(rows[0])
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["superseded"] != true || fields["Repo"] != repo.Name {
		t.Fatalf("history context %s", body)
	}
	if _, err := s.db.ExecContext(t.Context(), `UPDATE runs SET status='passed', finished_at=? WHERE id=?`, unixNano(now), newest.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListRecentRuns(t.Context(), filter)
	if err != nil || len(rows) != 0 {
		t.Fatalf("terminal successor still supersedes: %+v %v", rows, err)
	}
}
