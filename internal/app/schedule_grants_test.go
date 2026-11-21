package app_test

import (
	"context"
	"reflect"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// A schedule is admitted with the authority a push of its branch gets: the
// same repository, build file and branch tier, never the release file's grants.
func TestScheduleGrantsAreThoseOfAPushToItsBranch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := request()
	grantBuildAndRelease(t, f, r)
	push, schedule := pushAndScheduleScopes(t, f, r)
	if schedule.Trigger != string(model.TriggerSchedule) || schedule.Release || schedule.File != ".oberth/build.yaml" {
		t.Fatal("schedule scope differs from a branch build", schedule)
	}
	same := schedule
	same.Trigger = push.Trigger
	if !reflect.DeepEqual(push, same) {
		t.Fatal("schedule authority scope is not a push's", push, schedule)
	}
	authority, err := app.NewAuthority(app.AuthorityConfig{Namespace: "runs", ServerNamespace: "oberth", ArgoNamespace: "argo", Grants: f.runs.Grants,
		Templates:     func(context.Context, string) (*wf.WorkflowTemplate, error) { return nil, nil },
		EnsureAccount: func(context.Context, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	pushed, _, err := authority.Load(t.Context(), push)
	if err != nil {
		t.Fatal(err)
	}
	granted, _, err := authority.Load(t.Context(), schedule)
	if err != nil || !reflect.DeepEqual(granted.Grants, pushed.Grants) || len(granted.Grants) != 1 || granted.Grants[0].File != ".oberth/build.yaml" {
		t.Fatal("schedule grants differ from a push's", granted.Grants, pushed.Grants, err)
	}
}

func grantBuildAndRelease(t *testing.T, f *fixture, r app.Request) {
	t.Helper()
	for _, file := range []string{".oberth/build.yaml", ".oberth/release.yaml"} {
		grant := stateport.GrantChange{Kind: stateport.GrantStorePath, Repository: r.Repository, File: file, Step: "build", Path: "ci/token"}
		if _, err := f.runs.ChangeGrant(t.Context(), stateport.Action{Actor: r.Actor.Identity}, grant); err != nil {
			t.Fatal(err)
		}
	}
}

// pushAndScheduleScopes records the authority scope of a check of r's branch
// and of a schedule preparation on the same branch.
func pushAndScheduleScopes(t *testing.T, f *fixture, r app.Request) (app.Scope, app.Scope) {
	t.Helper()
	var scopes []app.Scope
	load := f.authority.LoadFunc
	f.authority.LoadFunc = func(ctx context.Context, s app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
		scopes = append(scopes, s)
		return load(ctx, s)
	}
	s := f.new(t)
	if _, err := s.Check(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	scheduled := r
	scheduled.ScheduleEntry = "nightly"
	if _, err := s.PrepareSchedule(t.Context(), scheduled); err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 {
		t.Fatal("authority was not loaded once per admission", scopes)
	}
	return scopes[0], scopes[1]
}
