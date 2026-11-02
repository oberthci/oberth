package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/store"
)

type resourceHistoryStub struct {
	calls  int
	filter store.RunResourceFilter
	repoID int64
}

func (s *resourceHistoryStub) RunResources(_ context.Context, id int64, f store.RunResourceFilter) (store.RunResourceHistory, error) {
	s.calls++
	s.repoID = id
	s.filter = f
	return store.RunResourceHistory{}, nil
}
func TestRunResourcesAuthorizesBeforeHistoryAndDefaults(t *testing.T) {
	f := newScopeFixture(t)
	reader := &resourceHistoryStub{}
	f.service.runResources = reader
	for _, repo := range []string{hiddenRepoName, "repo-99"} {
		raw, _ := json.Marshal(map[string]any{"repo": repo})
		if _, err := f.service.CallTool(t.Context(), scopedActor, "run_resources", raw); err == nil {
			t.Fatal("hidden repo allowed")
		}
	}
	if reader.calls != 0 {
		t.Fatal("unauthorized read")
	}
	got, err := f.call(scopedActor, "run_resources", map[string]any{"repo": f.visible.Name})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(measured.Report); !ok {
		t.Fatalf("type %T", got)
	}
	if reader.filter.Runs != 30 || reader.filter.Ref != f.visible.DefaultBranch || reader.filter.IncludeFailed || reader.repoID != f.visible.ID {
		t.Fatalf("defaults: %+v", reader)
	}
}
func TestRunResourcesRejectsBoundsAndHiddenExactRun(t *testing.T) {
	f := newScopeFixture(t)
	hiddenID := ""
	for _, runs := range f.runs {
		if len(runs) > 0 && runs[0].RepoID != f.visible.ID {
			hiddenID = runs[0].ID
			break
		}
	}
	reader := &resourceHistoryStub{}
	f.service.runResources = reader
	for _, args := range []map[string]any{{"repo": f.visible.Name, "last": 101}, {"repo": f.visible.Name, "last": -1}, {"repo": f.visible.Name, "format": "xml"}, {"repo": f.visible.Name, "run": hiddenID}, {"repo": f.visible.Name, "run": "garbage"}} {
		if _, err := f.call(scopedActor, "run_resources", args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if reader.calls != 0 {
		t.Fatal("invalid input reached history")
	}
}
