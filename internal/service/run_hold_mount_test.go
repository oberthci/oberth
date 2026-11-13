package service

import (
	"path"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestRunHoldPatchExecutesTheCompiledPodHelperMount(t *testing.T) {
	service, operator, _, run := holdFixture(t)
	config := argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "credentialed", CISecretsServiceAccount: "ci-secrets", ExecutorServiceAccount: "executor", Assist: argojob.AssistConfig{Enabled: true}}
	source := "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nspec:\n  entrypoint: test\n  templates:\n  - name: test\n    activeDeadlineSeconds: 600\n    container:\n      image: golang:1.26-alpine@sha256:" + strings.Repeat("a", 64) + "\n      command: [/bin/true]\n"
	request := argojob.Request{RunID: run.ID, RepositoryID: run.RepoID, Name: run.WorkflowName, Repo: "repo", UpstreamOrg: "org", Ref: run.Ref, SHA: strings.Repeat("a", 40), Trigger: pipeline.TriggerCI, Source: []byte(source), ApprovedSecrets: map[string]bool{}, Assist: true, SourceVolume: argojob.SourceVolume{ClaimName: "source-claim", SubPath: "src", BinarySubPath: "bin"}}
	workflow, err := argojob.Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	mounted := ""
	for _, mount := range workflow.Spec.Templates[0].Container.VolumeMounts {
		if mount.Name == argojob.SourceVolumeName && mount.SubPath == "bin" && mount.ReadOnly {
			mounted = path.Join(mount.MountPath, "oberth")
		}
	}
	if mounted == "" {
		t.Fatal("compiled Pod has no helper mount")
	}
	diff := "--- /dev/null\n+++ b/NOTE\n@@ -0,0 +1 @@\n+patched\n"
	if _, err = service.patchRunHold(t.Context(), api.Actor{Identity: "uplink-a"}, run.ID, "", diff); err != nil {
		t.Fatal(err)
	}
	if len(operator.execs) == 0 {
		t.Fatal("patch made no helper calls")
	}
	for _, argv := range operator.execs {
		if len(argv) < 2 || argv[0] != mounted || argv[1] != "internal-hold-fs" {
			t.Fatalf("service executes %v, compiled Pod mounts %s", argv, mounted)
		}
	}
}
