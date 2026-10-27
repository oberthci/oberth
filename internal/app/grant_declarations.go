package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// declarationSnapshot captures both documents before replacing observations;
// a missing file is empty, while an unreadable or invalid file preserves them.
type declarationSnapshot struct{ declarations []store.GrantDeclaration }

func (snapshot *declarationSnapshot) RecordGrantDeclarations(_ context.Context, _, _ string, declarations []store.GrantDeclaration) error {
	snapshot.declarations = append(snapshot.declarations, declarations...)
	return nil
}

func (jobs *ArgoJobs) recordCurrentDeclarations(ctx context.Context, request service.JobRequest) {
	jobs.mu.Lock()
	declarations := jobs.declarations
	jobs.mu.Unlock()
	recorder, ok := declarations.(interface {
		ReplaceGrantDeclarations(context.Context, string, string, []store.GrantDeclaration) error
	})
	if !ok {
		return
	}
	repo := declarationRepo(request)
	sha := request.Run.TestedSHA
	if sha == "" {
		sha = request.Run.SHA
	}
	stage := ""
	defer func() {
		if stage != "" {
			// Do not log source-bearing parse errors or document contents.
			slog.DebugContext(ctx, "grant declaration snapshot dropped", "repo", repo, "sha", sha, "stage", stage)
		}
	}()
	snapshot := &declarationSnapshot{}
	collector := &ArgoJobs{declarations: snapshot}
	for _, file := range append([]string{argoworkflow.BuildFile}, jobs.config.ReleaseTagRoutes.Files(repo)...) {
		trigger := periapsis.TriggerRelease
		if file == argoworkflow.BuildFile {
			trigger = periapsis.TriggerCI
		}
		stage = "read"
		source, err := readBoundedSourceFile(request.SourceDir, file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return
		}
		stage = "decode"
		wf, err := argoworkflow.Decode(source)
		if err != nil {
			return
		}
		refKind, runTrigger := model.RefBranch, "branch"
		if trigger == periapsis.TriggerRelease {
			refKind, runTrigger = model.RefTag, "release"
		}
		stage = "fragments"
		fragments, err := loadFragments(ctx, jobs.fragments, source, trigger, refKind, runTrigger)
		if err != nil {
			return
		}
		stage = "resolve"
		if _, err := argoworkflow.Resolve(wf, fragments); err != nil {
			return
		}
		stage = "paths"
		if _, err := argoworkflow.DeclaredSecretPaths(wf); err != nil {
			return
		}
		stage = "marshal"
		resolved, err := json.Marshal(wf)
		if err != nil {
			return
		}
		collector.recordDeclarations(ctx, request, resolved, trigger)
	}
	stage = "replace"
	if err := recorder.ReplaceGrantDeclarations(ctx, repo, sha, snapshot.declarations); err != nil {
		return
	}
	stage = ""
}

// Match Upstream.QualifiedRepo even when the upstream has no organization.
func declarationRepo(request service.JobRequest) string {
	return model.CanonicalRepoKey(request.UpstreamName, request.UpstreamOrg, request.Repository.Name)
}
