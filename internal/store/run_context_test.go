package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRunRepositoryContextSupersededLanes(t *testing.T) {
	for _, tc := range []struct {
		name, firstTrigger, newerTrigger, firstEntry, newerEntry string
		firstTag, newerTag, want                                 bool
	}{
		{name: "push then nightly on main", firstTrigger: "push", newerTrigger: "schedule", newerEntry: "nightly"},
		{name: "nightly then push on main", firstTrigger: "schedule", newerTrigger: "push", firstEntry: "nightly"},
		{name: "different schedule entries", firstTrigger: "schedule", newerTrigger: "schedule", firstEntry: "nightly", newerEntry: "weekly"},
		{name: "same schedule entry", firstTrigger: "schedule", newerTrigger: "schedule", firstEntry: "nightly", newerEntry: "nightly", want: true},
		{name: "same push lane", firstTrigger: "push", newerTrigger: "push", want: true},
		{name: "ordinary triggers share a lane", firstTrigger: "push", newerTrigger: "branch", want: true},
		{name: "legacy unnamed schedule shares push lane", firstTrigger: "schedule", newerTrigger: "push", want: true},
		{name: "push then promotion", firstTrigger: "push", newerTrigger: "promotion"},
		{name: "promotion then push", firstTrigger: "promotion", newerTrigger: "push"},
		{name: "independent promotion attempts", firstTrigger: "promotion", newerTrigger: "promotion"},
		{name: "independent tag attempts", firstTrigger: "tag", newerTrigger: "tag", firstTag: true, newerTag: true},
		{name: "branch then tag", firstTrigger: "push", newerTrigger: "tag", newerTag: true},
		{name: "tag then branch", firstTrigger: "tag", newerTrigger: "push", firstTag: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			// Reproduce a newer nightly run at an older SHA than the push head.
			spec := testRunSpec(repo.ID, "main", strings.Repeat("b", 40))
			spec.Trigger, spec.ScheduleEntry = tc.firstTrigger, tc.firstEntry
			if tc.firstTag {
				spec.RefKind = model.RefTag
			}
			first, err := s.EnqueueRun(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			spec.SHA = strings.Repeat("a", 40)
			spec.Trigger, spec.ScheduleEntry, spec.RefKind = tc.newerTrigger, tc.newerEntry, model.RefBranch
			if tc.newerTag {
				spec.RefKind = model.RefTag
			}
			newer, err := s.EnqueueRun(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			firstRead, err := s.ResolveRunID(t.Context(), first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want {
				if firstRead.Status != model.RunInterrupted || firstRead.SupersededBy != newer.ID {
					t.Fatalf("enqueue did not supersede same-lane run: %+v", firstRead)
				}
			} else if firstRead.Status != model.RunQueued || firstRead.SupersededBy != "" {
				t.Fatalf("enqueue changed independent run: %+v", firstRead)
			}
			if firstRead.Superseded != tc.want {
				t.Errorf("older run superseded=%t, want %t", firstRead.Superseded, tc.want)
			}
			newerRead, err := s.ResolveRunID(t.Context(), newer.ID)
			if err != nil || newerRead.Superseded {
				t.Fatalf("newest run superseded: %+v, %v", newerRead, err)
			}
			for _, view := range []string{"all", "active", "history", "latest"} {
				t.Run(view, func(t *testing.T) {
					filter := model.RunListFilter{Terminal: view == "history", LatestPerRef: view == "latest"}
					list := func() ([]model.Run, error) {
						if view == "active" {
							return s.ListActiveRuns(t.Context(), filter)
						}
						return s.ListRecentRuns(t.Context(), filter)
					}
					rows, err := list()
					if err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						if want := row.ID == first.ID && tc.want; row.Superseded != want {
							t.Errorf("run %s superseded=%t, want %t", row.ID, row.Superseded, want)
						}
					}
					filter.Superseded = true
					rows, err = list()
					if err != nil {
						t.Fatal(err)
					}
					wantCount := 0
					if tc.want && (view == "all" || view == "history") {
						wantCount = 1
					}
					if len(rows) != wantCount || (wantCount == 1 && rows[0].ID != first.ID) {
						t.Fatalf("superseded filter returned %+v, want %d rows", rows, wantCount)
					}
				})
			}
		})
	}
}

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
			if _, err := s.SetQueuePriorityPatterns(t.Context(), "admin", []string{"feature/*"}); err != nil {
				t.Fatal(err)
			}
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
