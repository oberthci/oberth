package app

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

func TestReleasePreflightRecordsDeniedCanonicalDeclarations(t *testing.T) {
	recorder := &fakeDeclarationRecorder{}
	jobs := &ArgoJobs{declarations: recorder}
	problems, err := jobs.PreflightRelease(context.Background(), service.JobRequest{
		Repository: model.Repository{ID: 42, Name: "repo"}, UpstreamName: "forge", UpstreamOrg: "org",
		Run: model.Run{SHA: "object", TestedSHA: "commit"},
	}, []byte(declarationWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 || !recorder.called || recorder.repo != "forge/org/repo" || recorder.sha != "commit" || len(recorder.declarations) != 2 {
		t.Fatalf("problems=%v recorder=%+v", problems, recorder)
	}
}

type changingPreflightGrants struct {
	grants map[string]map[string]bool
	calls  int
}

func (loader *changingPreflightGrants) ActiveSecretGrants(context.Context, int64) (map[string]map[string]bool, error) {
	loader.calls++
	return loader.grants, nil
}

func TestReleasePreflightReloadsGrantsAfterApproval(t *testing.T) {
	loader := &changingPreflightGrants{}
	jobs := &ArgoJobs{secretAccess: loader}
	request := service.JobRequest{Repository: model.Repository{ID: 42, Name: "repo"}, UpstreamName: "forge", UpstreamOrg: "org"}
	problems, err := jobs.PreflightRelease(context.Background(), request, []byte(declarationWorkflow))
	if err != nil || len(problems) == 0 {
		t.Fatalf("missing grant: %v %v", problems, err)
	}
	loader.grants = map[string]map[string]bool{"*": {"oberth/data/release/r2-token": true, "oberth/data/release/cosign-key": true}}
	problems, err = jobs.PreflightRelease(context.Background(), request, []byte(declarationWorkflow))
	if err != nil || len(problems) != 0 || loader.calls != 2 {
		t.Fatalf("fresh grants: %v %v calls=%d", problems, err, loader.calls)
	}
}
