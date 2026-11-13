package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestUnapprovedSecretBuildRefusalOffersYAMLAndOperatorRemedies(t *testing.T) {
	config := argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "credentialed", CISecretsServiceAccount: "ci-secrets", ExecutorServiceAccount: "executor"}
	source := "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nmetadata:\n  annotations:\n    oberth.ci/secret-paths: oberth/upstream/org/repo/credential\nspec:\n  entrypoint: test\n  templates:\n  - name: test\n    container:\n      image: golang:1.26-alpine@sha256:" + strings.Repeat("a", 64) + "\n      command: [oberth, secretstore, exec, --path=oberth/upstream/org/repo/credential, --, /bin/true]\n"
	_, err := argojob.Build(config, argojob.Request{RunID: "run-1", RepositoryID: 1, Name: "workflow", Repo: "repo", UpstreamOrg: "org", Ref: "refs/heads/branch", SHA: strings.Repeat("a", 40), Trigger: pipeline.TriggerCI, Source: []byte(source), ApprovedSecrets: map[string]bool{}})
	var refusal *argoworkflow.AdmissionError
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "unapproved secret path") {
		t.Fatal("fixture did not reach secret approval", err)
	}
	for _, message := range []string{"correct the Workflow secret path", "ask an operator", "oberth access allow"} {
		if !strings.Contains(err.Error(), message) {
			t.Fatal("refusal omitted remedy", message, err)
		}
	}
	run := model.Run{Status: model.RunFailed, Phase: "job", Error: refusal.Error(), FailureCause: validationRefusalPrefix + refusal.Error()}
	hint := runRetryHint(run, nil)
	if hint == nil || hint.Class != model.RetryCode || hint.Cause != "validation" || !strings.Contains(hint.Action, "Workflow YAML") || !strings.Contains(hint.Action, "operator") {
		t.Fatal("retry hint omitted approval remedy", hint)
	}
	run.Error = "argoworkflow: spec.hostNetwork is not admitted"
	run.FailureCause = validationRefusalPrefix + run.Error
	hint = runRetryHint(run, nil)
	if hint == nil || strings.Contains(hint.Action, "operator") {
		t.Fatal("forbidden content offered an operator bypass", hint)
	}
}
