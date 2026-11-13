package argojob

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// Snapshot only authored securityContext fields. Encoding each typed field
// before and after Build checks byte equality without conflating unrelated
// server metadata/identity changes with the security-context contract.
func securityContextSnapshot(t *testing.T, workflow *wfv1.Workflow) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(workflow.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var object any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	contexts := map[string]json.RawMessage{}
	var walk func(any, string)
	walk = func(value any, path string) {
		switch node := value.(type) {
		case map[string]any:
			for key, child := range node {
				next := path + "." + key
				if key == "securityContext" {
					encoded, err := json.Marshal(child)
					if err != nil {
						t.Fatal(err)
					}
					contexts[next] = encoded
				} else {
					walk(child, next)
				}
			}
		case []any:
			for i, child := range node {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(object, "spec")
	return contexts
}

func buildSecurityWorkflow(t *testing.T, authored *wfv1.Workflow) *wfv1.Workflow {
	t.Helper()
	before := securityContextSnapshot(t, authored)
	if err := argoworkflow.Admit(authored, argoworkflow.Policy{}); err != nil {
		t.Fatalf("authored security context refused: %v", err)
	}
	raw, err := json.Marshal(authored)
	if err != nil {
		t.Fatal(err)
	}
	built, err := Build(testConfig(), testRequest(pipeline.TriggerCI, string(raw)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	after := securityContextSnapshot(t, built)
	if len(before) != len(after) {
		t.Fatalf("securityContext count changed: before=%s after=%s", mustJSON(t, before), mustJSON(t, after))
	}
	for path, want := range before {
		if !bytes.Equal(want, after[path]) {
			t.Errorf("%s changed: before=%s after=%s", path, want, after[path])
		}
	}
	return built
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAuthoredSecurityContextsPassThrough(t *testing.T) {
	contexts := map[string]*corev1.SecurityContext{
		"privileged":       {Privileged: ptr.To(true)},
		"added-capability": {Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}},
		"escalation":       {AllowPrivilegeEscalation: ptr.To(true)},
		"unconfined":       {SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}},
		"run-as-user":      {RunAsUser: ptr.To(int64(1234))},
		"writable-root":    {ReadOnlyRootFilesystem: ptr.To(false)},
		"net-bind-service": {Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"NET_BIND_SERVICE"}}},
	}
	for _, shape := range []string{"main", "script", "init", "sidecar", "container-set", "template-defaults", "inline-dag", "inline-step"} {
		for name, security := range contexts {
			t.Run(shape+"/"+name, func(t *testing.T) {
				wf, err := argoworkflow.Decode([]byte(admissibleSecurityDocument))
				if err != nil {
					t.Fatal(err)
				}
				tmpl := &wf.Spec.Templates[0]
				container := tmpl.Container.DeepCopy()
				container.SecurityContext = security.DeepCopy()
				switch shape {
				case "main":
					tmpl.Container = container
				case "script":
					tmpl.Container = nil
					tmpl.Script = &wfv1.ScriptTemplate{Container: *container, Source: "true"}
				case "init":
					container.Name = "author-init"
					tmpl.InitContainers = []wfv1.UserContainer{{Container: *container}}
				case "sidecar":
					container.Name = "author-sidecar"
					tmpl.Sidecars = []wfv1.UserContainer{{Container: *container}}
				case "container-set":
					container.Name = "main"
					tmpl.Container = nil
					tmpl.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: *container}}}
				case "template-defaults":
					wf.Spec.TemplateDefaults = &wfv1.Template{Container: container}
				case "inline-dag":
					tmpl.Container = nil
					tmpl.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "child", Inline: &wfv1.Template{Container: container}}}}
				case "inline-step":
					tmpl.Container = nil
					tmpl.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "child", Inline: &wfv1.Template{Container: container}}}}}
				}
				buildSecurityWorkflow(t, wf)
			})
		}
	}
}

func TestAuthoredPodSecurityContextsPassThrough(t *testing.T) {
	for _, shape := range []string{"workflow", "template", "template-defaults", "inline"} {
		for name, security := range map[string]*corev1.PodSecurityContext{
			"unconfined": {SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}},
			"root":       {RunAsUser: ptr.To(int64(0)), RunAsNonRoot: ptr.To(false)},
			"nonroot":    {RunAsUser: ptr.To(int64(1234)), RunAsGroup: ptr.To(int64(2345)), RunAsNonRoot: ptr.To(true), FSGroup: ptr.To(int64(3456))},
		} {
			t.Run(shape+"/"+name, func(t *testing.T) {
				wf, err := argoworkflow.Decode([]byte(admissibleSecurityDocument))
				if err != nil {
					t.Fatal(err)
				}
				switch shape {
				case "workflow":
					wf.Spec.SecurityContext = security.DeepCopy()
				case "template":
					wf.Spec.Templates[0].SecurityContext = security.DeepCopy()
				case "template-defaults":
					wf.Spec.TemplateDefaults = &wfv1.Template{SecurityContext: security.DeepCopy()}
				case "inline":
					container := wf.Spec.Templates[0].Container
					wf.Spec.Templates[0].Container = nil
					wf.Spec.Templates[0].DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "child", Inline: &wfv1.Template{SecurityContext: security.DeepCopy(), Container: container}}}}
				}
				buildSecurityWorkflow(t, wf)
			})
		}
	}
}

func TestBuildAddsNoSecurityContextOrPrivateTmp(t *testing.T) {
	wf, err := argoworkflow.Decode([]byte(nestedSecurityDocument))
	if err != nil {
		t.Fatal(err)
	}
	// Include every container position and a native default in the omitted case.
	wf.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{Image: wf.Spec.Templates[1].Container.Image}}
	wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "set", ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main", Image: wf.Spec.Templates[1].Container.Image, Command: []string{"true"}}}}}})
	built := buildSecurityWorkflow(t, wf)
	if len(securityContextSnapshot(t, built)) != 0 {
		t.Fatal("security context injected")
	}
	for _, v := range built.Spec.Volumes {
		if v.Name == "oberth-tmp" || v.Name == "oberth-nonroot-tmp" {
			t.Fatalf("private tmp volume injected: %s", v.Name)
		}
	}
	walkTemplates(built, func(tmpl *wfv1.Template) {
		templateContainers(tmpl, func(c *corev1.Container) {
			for _, m := range c.VolumeMounts {
				if m.MountPath == "/tmp" {
					t.Errorf("template %s gained a /tmp mount", tmpl.Name)
				}
			}
		})
		var patch corev1.PodSpec
		if tmpl.PodSpecPatch != "" {
			if err := json.Unmarshal([]byte(tmpl.PodSpecPatch), &patch); err != nil {
				t.Fatal(err)
			}
		}
		if patch.SecurityContext != nil {
			t.Error("Pod patch injects securityContext")
		}
		for _, c := range append(patch.InitContainers, patch.Containers...) {
			if c.Name == "oberth-prepare-nonroot" {
				t.Error("nonroot prepare container injected")
			}
			if c.Name == executorTokenWaiter {
				if !reflect.DeepEqual(c.SecurityContext, nonrootContainerSecurity(0)) {
					t.Error("A7 waiter security context changed")
				}
			} else if c.SecurityContext != nil {
				t.Errorf("Argo helper %s gained securityContext", c.Name)
			}
		}
	})
}

func TestRemovedNonrootAnnotationRefused(t *testing.T) {
	for _, value := range []string{"test", "", "test,test", "{{workflow.name}}"} {
		t.Run(value, func(t *testing.T) {
			wf, err := argoworkflow.Decode([]byte(admissibleSecurityDocument))
			if err != nil {
				t.Fatal(err)
			}
			wf.Annotations = map[string]string{"oberth.ci/nonroot-templates": value}
			raw := mustJSON(t, wf)
			_, err = Build(testConfig(), testRequest(pipeline.TriggerCI, string(raw)))
			if err == nil || !strings.Contains(err.Error(), `metadata.annotations["oberth.ci/nonroot-templates"] is refused`) {
				t.Fatalf("removed annotation not refused: %v", err)
			}
		})
	}
}
