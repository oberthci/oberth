package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestReleaseWIFAuditBindsPublicPolicyAndSource(t *testing.T) {
	config := argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor",
		PerRepoIdentities: map[string]argojob.PerRepoIdentityConfig{"github/org/repo": {ServiceAccountName: "repo-release"}},
		ReleaseWIF:        &argojob.ReleaseWIFConfig{Namespace: "pipelines", Roles: map[string]argojob.ReleaseWIFRole{"image-writer": {Provider: "projects/123/locations/global/workloadIdentityPools/images/providers/issuer", ServiceAccount: "publisher@example-project.iam.gserviceaccount.com"}}, Repositories: map[string]argojob.ReleaseWIFRepository{"github/org/repo": {ServiceAccountName: "repo-release", Templates: map[string]string{"publish": "image-writer"}}}}}
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
	submission := argojob.Request{RunID: "run-wif", Name: "wif-test", Repo: "repo", UpstreamName: "github", UpstreamOrg: "org", Ref: "v1.0.0", SHA: strings.Repeat("a", 40), Trigger: periapsis.TriggerRelease, Source: []byte(document), ApprovedSecrets: map[string]bool{}}
	var captured model.AuditActionSpec
	jobs := &ArgoJobs{config: config, auditor: &stubAuditor{onAppend: func(s model.AuditActionSpec) { captured = s }}}
	request := service.JobRequest{Run: model.Run{ID: submission.RunID, Actor: "reviewer@host", Ref: "v1.0.0", SHA: strings.Repeat("b", 40)}, Repository: model.Repository{Name: "repo"}, UpstreamOrg: "org"}
	if err := jobs.auditSubmission(t.Context(), request, submission); err != nil {
		t.Fatal(err)
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(captured.Details), &details); err != nil {
		t.Fatal(err)
	}
	wf, err := argojob.Build(config, submission)
	if err != nil {
		t.Fatal(err)
	}
	if details["release_wif_policy_sha256"] != wf.Annotations["oberth.ci/release-wif-policy-sha256"] || details["workflow_spec_sha256"] != wf.Annotations["oberth.ci/spec-identity"] || details["sha"] != submission.SHA || details["object_sha"] != request.Run.SHA {
		t.Fatal("audit does not bind exact source, admitted spec and capability policy")
	}
	if details["release_wif_leaves"].(map[string]any)["publish"] != "image-writer" {
		t.Fatal("audit lost exact named capability")
	}
	captured = model.AuditActionSpec{}
	submission.Trigger = periapsis.TriggerCI
	if err := jobs.auditSubmission(t.Context(), request, submission); err == nil || captured.Action != "" {
		t.Fatal("branch obtained a release capability audit binding")
	}
}
