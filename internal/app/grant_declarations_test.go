package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

type snapshotRecorder struct {
	fakeDeclarationRecorder
	replacements int
}

func (s *snapshotRecorder) ReplaceGrantDeclarations(ctx context.Context, repo, sha string, declarations []store.GrantDeclaration) error {
	s.replacements++
	return s.RecordGrantDeclarations(ctx, repo, sha, declarations)
}

func TestCurrentDeclarationsKeepBothTiersAndClearRemovedPaths(t *testing.T) {
	_, dir := writeWorkflowSource(t, declarationWorkflow)
	release := filepath.Join(dir, ".oberth", "release.yaml")
	if err := os.WriteFile(release, []byte(declarationWorkflow), 0600); err != nil {
		t.Fatal(err)
	}
	recorder := &snapshotRecorder{}
	jobs := &ArgoJobs{declarations: recorder}
	request := service.JobRequest{SourceDir: dir, Repository: model.Repository{Name: "repo"}, UpstreamName: "forge", UpstreamOrg: "org", Run: model.Run{SHA: "old", TestedSHA: "new"}}
	jobs.recordCurrentDeclarations(context.Background(), request)
	if recorder.replacements != 1 || len(recorder.declarations) != 4 || recorder.repo != "forge/org/repo" || recorder.sha != "new" {
		t.Fatalf("%+v", recorder)
	}
	if err := os.Remove(release); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, ".oberth", "build.yaml")); err != nil {
		t.Fatal(err)
	}
	jobs.recordCurrentDeclarations(context.Background(), request)
	if recorder.replacements != 2 || len(recorder.declarations) != 0 {
		t.Fatalf("empty snapshot=%+v", recorder)
	}
	if err := os.WriteFile(release, []byte("invalid yaml : ["), 0600); err != nil {
		t.Fatal(err)
	}
	jobs.recordCurrentDeclarations(context.Background(), request)
	if recorder.replacements != 2 {
		t.Fatal("invalid source erased observations")
	}
}
