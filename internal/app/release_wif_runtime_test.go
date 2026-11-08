package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

// Release preflight and admission read the runtime layer, so a grant or a
// revoke changes the next decision without a Helm upgrade or restart (#1497).
func TestReleasePreflightFollowsRuntimeReleaseWIFOverrides(t *testing.T) {
	config := argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor",
		PerRepoIdentities: map[string]argojob.PerRepoIdentityConfig{"github/org/repo": {ServiceAccountName: "repo-release"}},
		ReleaseWIF:        &argojob.ReleaseWIFConfig{Namespace: "pipelines", Roles: map[string]argojob.ReleaseWIFRole{"image-writer": {Provider: "projects/123/locations/global/workloadIdentityPools/images/providers/issuer", ServiceAccount: "publisher@example-project.iam.gserviceaccount.com"}}}}
	const document = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  activeDeadlineSeconds: 600
  entrypoint: publish
  templates:
  - name: publish
    metadata:
      annotations:
        oberth.ci/release-wif-role: image-writer
    container:
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: ["true"]
`
	jobs := &ArgoJobs{config: config}
	runtime := argojob.NewReleaseWIFRuntime(config)
	jobs.SetReleaseWIFRuntime(runtime)
	request := service.WorkflowRequest{Run: model.Run{ID: "run", Ref: "v1.0.0", RefKind: model.RefTag, SHA: strings.Repeat("a", 40)}, Repository: model.Repository{ID: 1, Name: "repo"}, UpstreamName: "github", UpstreamOrg: "org"}
	plan := func() []string {
		t.Helper()
		result, err := jobs.PlanRelease(t.Context(), request, []byte(document))
		if err != nil {
			t.Fatal(err)
		}
		return result.Problems
	}
	if problems := plan(); len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), "not approved") {
		t.Fatalf("boot map without a grant must report the gap: %v", problems)
	}
	grant := model.ReleaseWIFOverride{Repo: "github/org/repo", Grant: json.RawMessage(`{"service_account_name":"repo-release","templates":{"publish":"image-writer"}}`)}
	if inactive := runtime.Apply([]model.ReleaseWIFOverride{grant}); len(inactive) != 0 {
		t.Fatalf("inactive: %+v", inactive)
	}
	if problems := plan(); len(problems) != 0 {
		t.Fatalf("runtime grant not visible to preflight: %v", problems)
	}
	runtime.Apply([]model.ReleaseWIFOverride{{Repo: "github/org/repo", Revoked: true}})
	if problems := plan(); len(problems) == 0 {
		t.Fatal("runtime revoke not visible to preflight")
	}
}
