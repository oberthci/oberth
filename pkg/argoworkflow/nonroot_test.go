package argoworkflow

import (
	"encoding/json"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func nonrootDocument(t *testing.T) *wfv1.Workflow {
	t.Helper()
	wf, err := Decode([]byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/nonroot-templates: test
spec:
  entrypoint: test
  activeDeadlineSeconds: 600
  templates:
  - name: test
    container:
      image: golang:1.26.6-trixie@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
`))
	if err != nil {
		t.Fatal(err)
	}
	return wf
}

func TestAdmitNonrootLeaves(t *testing.T) {
	for _, script := range []bool{false, true} {
		wf := nonrootDocument(t)
		if script {
			wf.Spec.Templates[0].Script = &wfv1.ScriptTemplate{Container: *wf.Spec.Templates[0].Container, Source: "true"}
			wf.Spec.Templates[0].Container = nil
		}
		if err := Admit(wf, Policy{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdmitRejectsUnsafeNonrootSelections(t *testing.T) {
	cases := map[string]func(*wfv1.Workflow){
		"empty":        func(w *wfv1.Workflow) { w.Annotations[NonrootTemplatesAnnotation] = "" },
		"duplicate":    func(w *wfv1.Workflow) { w.Annotations[NonrootTemplatesAnnotation] = "test,test" },
		"unknown":      func(w *wfv1.Workflow) { w.Annotations[NonrootTemplatesAnnotation] = "missing" },
		"dynamic":      func(w *wfv1.Workflow) { w.Annotations[NonrootTemplatesAnnotation] = "{{inputs.parameters.name}}" },
		"credentialed": func(w *wfv1.Workflow) { w.Annotations[SecretPathsAnnotation] = "oberth/upstream/owner/repo/token" },
		"archive-logs": func(w *wfv1.Workflow) { value := true; w.Spec.ArchiveLogs = &value },
		"defaults":     func(w *wfv1.Workflow) { w.Spec.TemplateDefaults = &wfv1.Template{} },
		"sidecar": func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Sidecars = []wfv1.UserContainer{{Container: *w.Spec.Templates[0].Container}}
		},
		"init": func(w *wfv1.Workflow) {
			w.Spec.Templates[0].InitContainers = []wfv1.UserContainer{{Container: *w.Spec.Templates[0].Container}}
		},
		"nested": func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container = nil
			w.Spec.Templates[0].DAG = &wfv1.DAGTemplate{}
		},
		"plugin":               func(w *wfv1.Workflow) { w.Spec.Templates[0].Plugin = &wfv1.Plugin{} },
		"artifact":             func(w *wfv1.Workflow) { w.Spec.Templates[0].Outputs.Artifacts = []wfv1.Artifact{{Name: "output"}} },
		"pod-patch":            func(w *wfv1.Workflow) { w.Spec.PodSpecPatch = `{"hostPID":true}` },
		"template-patch":       func(w *wfv1.Workflow) { w.Spec.Templates[0].PodSpecPatch = `{"hostPID":true}` },
		"security-context":     func(w *wfv1.Workflow) { w.Spec.Templates[0].Container.SecurityContext = &corev1.SecurityContext{} },
		"pod-security-context": func(w *wfv1.Workflow) { w.Spec.Templates[0].SecurityContext = &corev1.PodSecurityContext{} },
		"reserved-main-name":   func(w *wfv1.Workflow) { w.Spec.Templates[0].Container.Name = "wait" },
		"parameter-input":      func(w *wfv1.Workflow) { w.Spec.Templates[0].Inputs.Parameters = []wfv1.Parameter{{Name: "value"}} },
		"dynamic-command":      func(w *wfv1.Workflow) { w.Spec.Templates[0].Container.Args = []string{"{{workflow.parameters.value}}"} },
		"claim-collision": func(w *wfv1.Workflow) {
			w.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "var-run-argo"}}}
		},
		"volume-collision": func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "oberth-nonroot-tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			wf := nonrootDocument(t)
			change(wf)
			if err := Admit(wf, Policy{}); err == nil {
				t.Fatal("unsafe selection was admitted")
			}
		})
	}
}

func TestNonrootTemplateSizeBoundary(t *testing.T) {
	for _, delta := range []int{-1, 0, 1} {
		wf := nonrootDocument(t)
		tmpl := &wf.Spec.Templates[0]
		tmpl.Container.Args = []string{""}
		encoded, err := json.Marshal(tmpl)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.Container.Args[0] = strings.Repeat("x", MaxNonrootTemplateBytes-len(encoded)+delta)
		err = ValidateNonrootTemplateSize(tmpl)
		if (err != nil) != (delta > 0) {
			t.Fatalf("size delta%d: %v", delta, err)
		}
	}
}

func TestNonrootSelectorWhitespaceAndLimit(t *testing.T) {
	wf := nonrootDocument(t)
	wf.Annotations[NonrootTemplatesAnnotation] = " test "
	names, err := DeclaredNonrootTemplates(wf)
	if err != nil || len(names) != 1 || names[0] != "test" {
		t.Fatalf("names=%v err=%v", names, err)
	}
	wf.Annotations[NonrootTemplatesAnnotation] = strings.Repeat("test,", MaxTemplates) + "test"
	if _, err := DeclaredNonrootTemplates(wf); err == nil {
		t.Fatal("unbounded selection admitted")
	}
}
