package argojob

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func changeCredentialOutputRequest(t *testing.T, request Request, change func(*wfv1.Workflow)) Request {
	t.Helper()
	wf, err := argoworkflow.Decode(request.Source)
	if err != nil {
		t.Fatal(err)
	}
	change(wf)
	request.Source, err = json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestCredentialOutputEffectiveDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, base, leaf string
		refused          bool
	}{
		{"safe-override", "/shared/data/result", "/root-result", false},
		{"mounted-override", "/root-result", "/shared/data/result", true},
		{"inherited-mounted", "/shared/data/result", "", true},
		{"inherited-safe", "/root-result", "", false},
		{"no-default", "", "/root-result", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, request := credentialOutputRequest(t, false, false, tc.leaf)
			request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) {
				if tc.leaf == "" {
					wf.Spec.Templates[0].Outputs = wfv1.Outputs{}
				}
				if tc.base != "" {
					wf.Spec.TemplateDefaults = &wfv1.Template{Outputs: wfv1.Outputs{Parameters: []wfv1.Parameter{{Name: "result", ValueFrom: &wfv1.ValueFrom{Path: tc.base}}}}}
				}
			})
			_, err := Build(config, request)
			if (err != nil) != tc.refused || (err != nil && !strings.Contains(err.Error(), "output read path")) {
				t.Fatalf("effective output admission: %v", err)
			}
		})
	}
	// Compare nested field inheritance and distinct output names against Argo's
	// whole-Template merge, independently of the production outputs-only merge.
	defaults := &wfv1.Template{Outputs: wfv1.Outputs{Parameters: []wfv1.Parameter{
		{Name: "result", ValueFrom: &wfv1.ValueFrom{Path: "/shared/data/result", Default: wfv1.AnyStringPtr("fallback")}},
		{Name: "inherited", ValueFrom: &wfv1.ValueFrom{Path: "/other"}},
	}, Artifacts: wfv1.Artifacts{{Name: "artifact", Path: "/shared/data/artifact"}}}}
	leaf := &wfv1.Template{Outputs: wfv1.Outputs{Parameters: []wfv1.Parameter{{Name: "result", ValueFrom: &wfv1.ValueFrom{Path: "/root-result"}}}, Artifacts: wfv1.Artifacts{{Name: "artifact", Path: "/root-artifact"}}}}
	before, _ := json.Marshal(defaults)
	base, _ := json.Marshal(defaults)
	patch, _ := json.Marshal(leaf)
	merged, err := strategicpatch.StrategicMergePatch(base, patch, wfv1.Template{})
	if err != nil {
		t.Fatal(err)
	}
	var native wfv1.Template
	if err := json.Unmarshal(merged, &native); err != nil {
		t.Fatal(err)
	}
	got, err := effectiveCredentialOutputs(leaf.Outputs, defaults)
	if err != nil || !reflect.DeepEqual(got, native.Outputs) {
		t.Fatalf("native merge differs: %+v, %v", got, err)
	}
	after, _ := json.Marshal(defaults)
	if string(before) != string(after) {
		t.Fatal("defaults mutated")
	}
}

func TestCredentialRelativeOutputPathsRefused(t *testing.T) {
	for _, cloud := range []bool{false, true} {
		for _, script := range []bool{false, true} {
			for _, output := range []string{"result", "./result", "../result"} {
				t.Run(fmt.Sprintf("cloud=%t/script=%t/%s", cloud, script, output), func(t *testing.T) {
					config, request := credentialOutputRequest(t, cloud, script, output)
					request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) {
						templateContainers(&wf.Spec.Templates[0], func(c *corev1.Container) { c.WorkingDir = "/shared/data" })
					})
					_, err := Build(config, request)
					if err == nil || !strings.Contains(err.Error(), `output "result"`) || !strings.Contains(err.Error(), "use an absolute path on the step's own filesystem") {
						t.Fatalf("relative output admitted or missing remedy: %v", err)
					}
				})
			}
		}
	}
}

func credentialOtherContainer(t *wfv1.Template, shape string) {
	if shape == "sidecar" {
		t.Sidecars = []wfv1.UserContainer{{Container: corev1.Container{Name: "other", Image: "example.invalid/other", Command: []string{"true"}}}}
	} else {
		main := *t.Container
		main.Name = "main"
		t.Container = nil
		t.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: main}, {Container: corev1.Container{Name: "other", Image: main.Image, Command: []string{"true"}}}}}
	}
}

func TestCredentialOtherContainersPathOutputsRefused(t *testing.T) {
	for _, cloud := range []bool{false, true} {
		for _, shape := range []string{"sidecar", "containerSet"} {
			t.Run(fmt.Sprintf("cloud=%t/%s", cloud, shape), func(t *testing.T) {
				config, request := credentialOutputRequest(t, cloud, false, "/root-result")
				request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) { credentialOtherContainer(&wf.Spec.Templates[0], shape) })
				var err error
				if shape == "containerSet" {
					// Existing secret delivery recognizes only Container/Script;
					// cloud identities refuse containerSet before mount injection.
					// Exercise the conditional guard with an explicit mount decision.
					wf, decodeErr := argoworkflow.Decode(request.Source)
					if decodeErr != nil {
						t.Fatal(decodeErr)
					}
					leaf := &wf.Spec.Templates[0]
					if cloud {
						err = validateCredentialOutputPaths(wf, nil, []*wfv1.Template{leaf})
					} else {
						err = validateCredentialOutputPaths(wf, map[*wfv1.Template]bool{leaf: true}, nil)
						built, buildErr := Build(config, request)
						if buildErr != nil {
							t.Fatalf("uncredentialed containerSet refused: %v", buildErr)
						}
						for _, m := range templateMainContainer(&built.Spec.Templates[0]).VolumeMounts {
							if m.Name == SecretsVolumeName || m.Name == ReleaseTokenVolumeName {
								t.Fatal("containerSet unexpectedly received store credentials")
							}
						}
					}
				} else {
					_, err = Build(config, request)
				}
				if err == nil || !strings.Contains(err.Error(), "another container of the Pod can redirect what Argo reads") || !strings.Contains(err.Error(), `template "main"`) {
					t.Fatalf("shared parameter read admitted: %v", err)
				}
			})
		}
	}
}

func TestCredentialOtherContainersResultReferencesRefused(t *testing.T) {
	for _, shape := range []string{"sidecar", "containerSet"} {
		for _, graph := range []string{"steps", "tasks", "inline-steps", "inline-tasks"} {
			for _, reference := range []string{"%s.alias.outputs.result", "%s['alias'].outputs.result", `%s["alias"].outputs.result`, "%s.alias.outputs.resultSuffix", "%s['alias'].outputsXresult"} {
				t.Run(shape+"/"+graph+"/"+reference, func(t *testing.T) {
					target := wfv1.Template{Name: "credential-leaf", Container: &corev1.Container{Image: "example.invalid/main"}}
					credentialOtherContainer(&target, shape)
					wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{target, {Name: "parent"}}}}
					leaf := &wf.Spec.Templates[0]
					parent := &wf.Spec.Templates[1]
					name, inline := leaf.Name, (*wfv1.Template)(nil)
					if strings.HasPrefix(graph, "inline-") {
						inline = leaf.DeepCopy()
						inline.Name = ""
						leaf = inline
						name = ""
					}
					prefix := "steps"
					if strings.HasSuffix(graph, "tasks") {
						prefix = "tasks"
						parent.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "alias", Template: name, Inline: inline}}}
					} else {
						parent.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "alias", Template: name, Inline: inline}}}}
					}
					// Argo scans every field of the invoking parent.
					parent.Metadata.Annotations = map[string]string{"reference": "{{=" + fmt.Sprintf(reference, prefix) + "}}"}
					for _, cloud := range []bool{false, true} {
						granted := map[*wfv1.Template]bool{leaf: !cloud}
						var identities []*wfv1.Template
						if cloud {
							identities = []*wfv1.Template{leaf}
						}
						err := validateCredentialOutputPaths(wf, granted, identities)
						if err == nil || !strings.Contains(err.Error(), "another container of the Pod can redirect what Argo reads") {
							t.Fatalf("shared result read admitted: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestCredentialOtherContainerOutputControls(t *testing.T) {
	for _, shape := range []string{"sidecar", "containerSet"} {
		for _, control := range []string{"no-outputs", "literal-parameter", "unrelated-result", "reused-alias", "single-container", "ungranted-path", "ungranted-result", "init-only"} {
			t.Run(shape+"/"+control, func(t *testing.T) {
				target := wfv1.Template{Name: "credential-leaf", Container: &corev1.Container{Image: "example.invalid/main"}}
				credentialOtherContainer(&target, shape)
				wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{target, {Name: "parent", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "alias", Template: target.Name}, {Name: "other", Template: "ungranted"}}}}}}}
				leaf := &wf.Spec.Templates[0]
				granted := map[*wfv1.Template]bool{leaf: true}
				switch control {
				case "literal-parameter":
					leaf.Outputs.Parameters = []wfv1.Parameter{{Name: "literal", Value: wfv1.AnyStringPtr("value")}}
				case "unrelated-result":
					wf.Spec.Templates[1].Metadata.Annotations = map[string]string{"ref": "{{tasks.other.outputs.result}}"}
				case "reused-alias":
					wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "unrelated-parent", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "alias", Template: "ungranted"}}}, Metadata: wfv1.Metadata{Annotations: map[string]string{"ref": "{{tasks.alias.outputs.result}}"}}})
					leaf = &wf.Spec.Templates[0]
					granted = map[*wfv1.Template]bool{leaf: true}
				case "single-container", "init-only":
					leaf.Sidecars = nil
					if leaf.ContainerSet != nil {
						leaf.ContainerSet.Containers = leaf.ContainerSet.Containers[:1]
					}
					if control == "init-only" {
						leaf.InitContainers = []wfv1.UserContainer{{Container: corev1.Container{Name: "init"}}}
					}
					leaf.Outputs.Parameters = []wfv1.Parameter{{Name: "result", ValueFrom: &wfv1.ValueFrom{Path: "/root-result"}}}
					wf.Spec.Templates[1].Metadata.Annotations = map[string]string{"ref": "{{tasks.alias.outputs.result}}"}
				case "ungranted-path":
					granted[leaf] = false
					leaf.Outputs.Parameters = []wfv1.Parameter{{Name: "result", ValueFrom: &wfv1.ValueFrom{Path: "relative-result"}}}
				case "ungranted-result":
					granted[leaf] = false
					wf.Spec.Templates[1].Metadata.Annotations = map[string]string{"ref": "{{tasks.alias.outputs.result}}"}
				}
				if err := validateCredentialOutputPaths(wf, granted, nil); err != nil {
					t.Fatalf("safe combination refused: %v", err)
				}
			})
		}
	}
}

func TestCredentialOutputGuardOwnPipelines(t *testing.T) {
	for _, location := range []string{"../../.oberth/", "testdata/own-pipelines-next/"} {
		for _, name := range []string{"build.yaml", "release.yaml"} {
			t.Run(location+name, func(t *testing.T) {
				raw, err := os.ReadFile(location + name)
				if err != nil {
					t.Fatal(err)
				}
				wf, err := argoworkflow.Decode(raw)
				if err != nil {
					t.Fatal(err)
				}
				// This guard's interaction with both deployed and next documents is
				// checked independently of the one known removed-annotation refusal.
				delete(wf.Annotations, "oberth.ci/nonroot-templates")
				raw, err = json.Marshal(wf)
				if err != nil {
					t.Fatal(err)
				}
				trigger, ref := pipeline.TriggerCI, "refs/heads/main"
				if name == "release.yaml" {
					trigger, ref = pipeline.TriggerRelease, "refs/tags/v0.1.0"
				}
				_, err = buildOberthPipeline(oberthConfig(), Request{RunID: "run-abc123", Name: "oberth-oberth-run-abc1-aabbccddeeff", Repo: "oberth", UpstreamOrg: "acme", UpstreamName: "github", Ref: ref, SHA: testSHA, Trigger: trigger, Source: raw, ApprovedSecrets: oberthReleaseApprovedSecrets()})
				if err != nil {
					t.Fatalf("own pipeline refused: %v", err)
				}
			})
		}
	}
}

func TestCredentialOtherContainersResultBuildRefused(t *testing.T) {
	for _, cloud := range []bool{false, true} {
		for _, script := range []bool{false, true} {
			t.Run(fmt.Sprintf("cloud=%t/script=%t", cloud, script), func(t *testing.T) {
				config, request := credentialOutputRequest(t, cloud, script, "/root-result")
				request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) {
					leaf := &wf.Spec.Templates[0]
					leaf.Outputs = wfv1.Outputs{}
					credentialOtherContainer(leaf, "sidecar")
					wf.Spec.Entrypoint = "parent"
					wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "parent", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "alias", Template: leaf.Name}}}, Outputs: wfv1.Outputs{Parameters: []wfv1.Parameter{{Name: "result", ValueFrom: &wfv1.ValueFrom{Parameter: "{{tasks.alias.outputs.result}}"}}}}})
				})
				_, err := Build(config, request)
				if err == nil || !strings.Contains(err.Error(), `template "main"`) || !strings.Contains(err.Error(), "another container of the Pod can redirect what Argo reads") {
					t.Fatalf("referenced result admitted: %v", err)
				}
			})
		}
	}
}

func TestCredentialOtherContainersInheritedResultRefused(t *testing.T) {
	leaf := wfv1.Template{Name: "credential-leaf", Container: &corev1.Container{Image: "example.invalid/main"}}
	credentialOtherContainer(&leaf, "sidecar")
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{leaf, {Name: "parent", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "alias", Template: leaf.Name}}}}}, TemplateDefaults: &wfv1.Template{Outputs: wfv1.Outputs{Parameters: []wfv1.Parameter{{Name: "forward", ValueFrom: &wfv1.ValueFrom{Parameter: "{{tasks.alias.outputs.result}}"}}}}}}}
	granted := map[*wfv1.Template]bool{&wf.Spec.Templates[0]: true}
	if err := validateCredentialOutputPaths(wf, granted, nil); err == nil || !strings.Contains(err.Error(), "another container of the Pod can redirect what Argo reads") {
		t.Fatalf("inherited result read admitted: %v", err)
	}
	// A leaf declaration overrides the inherited parent reference natively.
	wf.Spec.Templates[1].Outputs.Parameters = []wfv1.Parameter{{Name: "forward", ValueFrom: &wfv1.ValueFrom{Parameter: "{{tasks.unrelated.outputs.result}}"}}}
	if err := validateCredentialOutputPaths(wf, granted, nil); err != nil {
		t.Fatalf("overridden inherited reference refused: %v", err)
	}
}
