package fake_test

import (
	"context"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stateport/contracttest"
	"github.com/oberthci/oberth/internal/stateport/fake"
)

func TestServiceRunContract(t *testing.T) { contracttest.RunSuite(t, runFixture) }
func runFixture(t *testing.T) contracttest.RunFixture {
	t.Helper()
	f := fake.NewRuns(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })
	seed := func(t *testing.T, name string) {
		t.Helper()
		parts := strings.Split(name, "/")
		f.SeedRepository(stateport.Repository{Upstream: parts[0], Name: strings.Join(parts[1:], "/"), Canonical: name})
	}
	seed(t, "origin/team/repo")
	return contracttest.RunFixture{Port: f, Admit: runAdmission, Repository: seed,
		Nodes: func(_ *testing.T, id string, nodes []stateport.Node) { f.SeedNodes(id, nodes) },
		Prune: func(t *testing.T) {
			if err := f.PruneEvents(t.Context()); err != nil {
				t.Fatal(err)
			}
		},
		Archive: func(t *testing.T, name string) {
			r, err := f.Repository(t.Context(), name, nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Archived = true
			f.SeedRepository(r)
		},
		NativeAdvance: func(t *testing.T, id string) {
			t.Helper()
			if err := f.AdvanceNative(t.Context(), id); err != nil {
				t.Fatal(err)
			}
		},
		Audit: func(t *testing.T) []model.AuditAction { t.Helper(); return f.Audits() }, Publication: f.Publication,
		Emit: func(ctx context.Context, events []stateport.EventWrite) error {
			return f.RecordEvents(ctx, events)
		},
	}
}
func runAdmission(t *testing.T, id string, r model.CIRequest) *admissioncontract.PreparedAdmission {
	t.Helper()
	v := admissioncontract.PreparedView{RunID: id, Repository: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: *r.CommitSHA, Trigger: r.Trigger, ScheduleEntry: r.ScheduleEntry, Workflow: &wfv1.Workflow{}, Plan: admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "build", Name: "build", HoldEligible: true, HoldArmed: r.Assist}}}}
	p, err := admissioncontract.Seal(v)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
