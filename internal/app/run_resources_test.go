package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

type resourceBlobStub struct {
	source []byte
	sha    string
}

func (b *resourceBlobStub) ReadOptionalBlob(_ context.Context, _ string, sha, _ string, _ int) ([]byte, bool, error) {
	b.sha = sha
	return b.source, true, nil
}
func (b *resourceBlobStub) ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error) {
	return "", nil, false, nil
}
func TestRunResourcesDeclarationsUseExactTestedCommitAndOnlyNumbers(t *testing.T) {
	source := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  generateName: numeric-
spec:
  entrypoint: root
  activeDeadlineSeconds: 600
  templates:
  - name: root
    dag:
      tasks:
      - name: step-12345678
        template: step-abc12345
  - name: step-abc12345
    activeDeadlineSeconds: 120
    container:
      image: example.invalid/repo-01
      command: [echo, SECRET-COMMAND]
      env:
      - name: SECRET-ENV
        value: hidden
      resources:
        requests: {cpu: 200m, memory: 128Mi}
        limits: {memory: 512Mi}
`)
	b := &resourceBlobStub{source: source}
	jobs := &ArgoJobs{resourceBlobs: b}
	got, e := jobs.ResourceDeclarations(t.Context(), service.WorkflowRequest{Run: model.Run{ID: strings.Repeat("a", 32), SHA: strings.Repeat("b", 40), TestedSHA: strings.Repeat("c", 40), RefKind: model.RefBranch, Trigger: "branch"}, Repository: model.Repository{ID: 1, Name: "repo-01"}, UpstreamName: "repo-01", UpstreamOrg: "repo-01"})
	if e != nil {
		t.Fatal(e)
	}
	if b.sha != strings.Repeat("c", 40) || len(got) != 1 || got[0].Template != "step-abc12345" || got[0].Stage != "step-12345678" || got[0].Resources["requests"]["cpu"] != "200m" || got[0].ActiveDeadlineSeconds == nil || *got[0].ActiveDeadlineSeconds != 120 {
		t.Fatalf("declarations: %+v", got)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "hidden") {
		t.Fatal("non-numeric spec fields leaked")
	}
}
