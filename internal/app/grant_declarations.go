package app

import (
	"context"
	"encoding/json"
	"errors"
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
	snapshot := &declarationSnapshot{}
	collector := &ArgoJobs{declarations: snapshot}
	for _, trigger := range []periapsis.Trigger{periapsis.TriggerCI, periapsis.TriggerRelease} {
		source, err := readArgoSource(request.SourceDir, trigger)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return
		}
		wf, err := argoworkflow.Decode(source)
		if err != nil {
			return
		}
		refKind, runTrigger := model.RefBranch, "branch"
		if trigger == periapsis.TriggerRelease {
			refKind, runTrigger = model.RefTag, "release"
		}
		fragments, err := loadFragments(ctx, jobs.fragments, source, trigger, refKind, runTrigger)
		if err != nil {
			return
		}
		if _, err := argoworkflow.Resolve(wf, fragments); err != nil {
			return
		}
		if _, err := argoworkflow.DeclaredSecretPaths(wf); err != nil {
			return
		}
		resolved, err := json.Marshal(wf)
		if err != nil {
			return
		}
		collector.recordDeclarations(ctx, request, resolved, trigger)
	}
	repo := request.Repository.Name
	if request.UpstreamName != "" && request.UpstreamOrg != "" {
		repo = request.UpstreamName + "/" + request.UpstreamOrg + "/" + repo
	}
	sha := request.Run.TestedSHA
	if sha == "" {
		sha = request.Run.SHA
	}
	_ = recorder.ReplaceGrantDeclarations(ctx, repo, sha, snapshot.declarations)
}
