package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
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

func TestDeclarationRepositoryMatchesUpstreamFallback(t *testing.T) {
	for _, upstream := range []model.Upstream{{Name: "forge", BaseURL: "ssh://forge.example/org"}, {Name: "forge"}} {
		t.Run(upstream.BaseURL, func(t *testing.T) {
			source, dir := writeWorkflowSource(t, declarationWorkflow)
			recorder := &snapshotRecorder{}
			jobs := &ArgoJobs{declarations: recorder}
			request := service.JobRequest{SourceDir: dir, Repository: model.Repository{Name: "repo"}, UpstreamName: upstream.Name, UpstreamOrg: upstream.Org(), Run: model.Run{SHA: "commit"}}
			want := upstream.QualifiedRepo("repo")
			jobs.recordDeclarations(context.Background(), request, source, periapsis.TriggerCI)
			if !recorder.called || recorder.repo != want {
				t.Fatalf("observation repo=%q want=%q", recorder.repo, want)
			}
			jobs.recordCurrentDeclarations(context.Background(), request)
			if recorder.replacements != 1 || recorder.repo != want {
				t.Fatalf("snapshot repo=%q want=%q", recorder.repo, want)
			}
		})
	}
}

func TestDroppedDeclarationSnapshotDebugLog(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, stage := range []string{"decode", "replace"} {
		t.Run(stage, func(t *testing.T) {
			output.Reset()
			source := declarationWorkflow
			recorder := &snapshotRecorder{}
			if stage == "decode" {
				source = "private-source-marker: ["
			} else {
				recorder.err = errors.New("private-source-marker")
			}
			_, dir := writeWorkflowSource(t, source)
			jobs := &ArgoJobs{declarations: recorder}
			jobs.recordCurrentDeclarations(context.Background(), service.JobRequest{SourceDir: dir, Repository: model.Repository{Name: "repo"}, Run: model.Run{SHA: "commit"}})
			if !strings.Contains(output.String(), "grant declaration snapshot dropped") || !strings.Contains(output.String(), "stage="+stage) || strings.Contains(output.String(), "private-source-marker") {
				t.Fatalf("unsafe or missing debug diagnostic: %s", output.String())
			}
		})
	}
}
