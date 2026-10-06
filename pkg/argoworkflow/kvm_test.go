package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func kvmDocument(t *testing.T) *wfv1.Workflow {
	t.Helper()
	wf, err := Decode([]byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/kvm-templates: kernel-test
spec:
  entrypoint: pipeline
  activeDeadlineSeconds: 600
  templates:
  - name: pipeline
    dag:
      tasks:
      - name: test
        template: kernel-test
  - name: kernel-test
    metadata:
      annotations:
        oberth.ci/workspace-mounts: "none"
        oberth.ci/workspace-env: "none"
    automountServiceAccountToken: false
    inputs:
      parameters:
      - name: kernel-version
        default: "6.12"
      - name: arch
        default: amd64
    container:
      image: golang:1.26.6-trixie@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/sh, -c]
      args: ["echo running kernel test for {{inputs.parameters.kernel-version}}"]
`))
	if err != nil {
		t.Fatal(err)
	}
	return wf
}

// TestAdmitKVMLeaves proves that a valid KVM leaf with parameters passes
// both DeclaredKVMTemplates and the full Admit gate.
func TestAdmitKVMLeaves(t *testing.T) {
	for _, script := range []bool{false, true} {
		wf := kvmDocument(t)
		if script {
			tmpl := templateByName(t, wf, "kernel-test")
			tmpl.Script = &wfv1.ScriptTemplate{Container: *tmpl.Container, Source: "true"}
			tmpl.Script.Command = []string{"/bin/sh"}
			tmpl.Container = nil
		}
		names, err := DeclaredKVMTemplates(wf)
		if err != nil {
			t.Fatalf("script=%v: %v", script, err)
		}
		if len(names) != 1 || names[0] != "kernel-test" {
			t.Fatalf("script=%v: names=%v", script, names)
		}
		if err := Admit(wf, Policy{}); err != nil {
			t.Fatalf("script=%v admit: %v", script, err)
		}
	}
}

// TestAdmitKVMParametersAllowed proves inputs.parameters are permitted on
// KVM leaves, unlike nonroot leaves which refuse them.
func TestAdmitKVMParametersAllowed(t *testing.T) {
	wf := kvmDocument(t)
	tmpl := templateByName(t, wf, "kernel-test")
	if len(tmpl.Inputs.Parameters) == 0 {
		t.Fatal("fixture should have parameters")
	}
	names, err := DeclaredKVMTemplates(wf)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Fatalf("names=%v", names)
	}
}

func TestAdmitRejectsUnsafeKVMSelections(t *testing.T) {
	cases := map[string]func(*wfv1.Workflow){
		"unknown-template": func(w *wfv1.Workflow) {
			w.Annotations[KVMTemplatesAnnotation] = "nonexistent"
		},
		"dag-template": func(w *wfv1.Workflow) {
			w.Annotations[KVMTemplatesAnnotation] = "pipeline"
		},
		"workspace-mounts-not-none": func(w *wfv1.Workflow) {
			tmpl := templateByNameMut(w, "kernel-test")
			tmpl.Metadata.Annotations[WorkspaceMountsAnnotation] = "readonly"
		},
		"workspace-mounts-missing": func(w *wfv1.Workflow) {
			tmpl := templateByNameMut(w, "kernel-test")
			delete(tmpl.Metadata.Annotations, WorkspaceMountsAnnotation)
		},
		"workspace-env-missing": func(w *wfv1.Workflow) {
			tmpl := templateByNameMut(w, "kernel-test")
			delete(tmpl.Metadata.Annotations, WorkspaceEnvAnnotation)
		},
		"token-automounted": func(w *wfv1.Workflow) {
			tmpl := templateByNameMut(w, "kernel-test")
			tmpl.AutomountServiceAccountToken = ptr.To(true)
		},
		"token-nil": func(w *wfv1.Workflow) {
			tmpl := templateByNameMut(w, "kernel-test")
			tmpl.AutomountServiceAccountToken = nil
		},
		"nonroot-overlap": func(w *wfv1.Workflow) {
			w.Annotations[NonrootTemplatesAnnotation] = "kernel-test"
		},
		"template-defaults": func(w *wfv1.Workflow) {
			w.Spec.TemplateDefaults = &wfv1.Template{}
		},
		"credentialed-workflow": func(w *wfv1.Workflow) {
			w.Annotations[SecretPathsAnnotation] = "oberth/upstream/owner/repo/token"
		},
		"wif-leaf": func(w *wfv1.Workflow) {
			tmpl := templateByNameMut(w, "kernel-test")
			tmpl.Metadata.Annotations[ReleaseWIFRoleAnnotation] = "image-writer"
		},
		"repo-declared-kvm-resource": func(w *wfv1.Workflow) {
			tmpl := templateByNameMut(w, "kernel-test")
			if tmpl.Container.Resources.Requests == nil {
				tmpl.Container.Resources.Requests = corev1.ResourceList{}
			}
			tmpl.Container.Resources.Requests["devices.kubevirt.io/kvm"] = resource.MustParse("1")
		},
		"repo-declared-kvm-limits": func(w *wfv1.Workflow) {
			tmpl := templateByNameMut(w, "kernel-test")
			if tmpl.Container.Resources.Limits == nil {
				tmpl.Container.Resources.Limits = corev1.ResourceList{}
			}
			tmpl.Container.Resources.Limits["devices.kubevirt.io/kvm"] = resource.MustParse("1")
		},
		"duplicate-name": func(w *wfv1.Workflow) {
			w.Annotations[KVMTemplatesAnnotation] = "kernel-test,kernel-test"
		},
		"invalid-name": func(w *wfv1.Workflow) {
			w.Annotations[KVMTemplatesAnnotation] = "INVALID_NAME"
		},
		"sidecar": func(w *wfv1.Workflow) {
			tmpl := templateByNameMut(w, "kernel-test")
			tmpl.Sidecars = []wfv1.UserContainer{{Container: *tmpl.Container}}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			wf := kvmDocument(t)
			change(wf)
			if _, err := DeclaredKVMTemplates(wf); err == nil {
				t.Fatal("unsafe KVM selection was admitted")
			}
		})
	}
}

// TestAdmitKVMWhitespaceAndLimit mirrors the nonroot tests.
func TestAdmitKVMWhitespaceAndLimit(t *testing.T) {
	wf := kvmDocument(t)
	wf.Annotations[KVMTemplatesAnnotation] = " kernel-test "
	names, err := DeclaredKVMTemplates(wf)
	if err != nil || len(names) != 1 || names[0] != "kernel-test" {
		t.Fatalf("names=%v err=%v", names, err)
	}
	wf.Annotations[KVMTemplatesAnnotation] = strings.Repeat("kernel-test,", MaxTemplates) + "kernel-test"
	if _, err := DeclaredKVMTemplates(wf); err == nil {
		t.Fatal("unbounded KVM selection admitted")
	}
}

// TestAdmitKVMNotDeclared proves absent annotation returns nil, nil.
func TestAdmitKVMNotDeclared(t *testing.T) {
	wf := kvmDocument(t)
	delete(wf.Annotations, KVMTemplatesAnnotation)
	names, err := DeclaredKVMTemplates(wf)
	if err != nil || names != nil {
		t.Fatalf("names=%v err=%v", names, err)
	}
}

func templateByName(t *testing.T, wf *wfv1.Workflow, name string) *wfv1.Template {
	t.Helper()
	for i := range wf.Spec.Templates {
		if wf.Spec.Templates[i].Name == name {
			return &wf.Spec.Templates[i]
		}
	}
	t.Fatalf("template %q not found", name)
	return nil
}

func templateByNameMut(wf *wfv1.Workflow, name string) *wfv1.Template {
	for i := range wf.Spec.Templates {
		if wf.Spec.Templates[i].Name == name {
			return &wf.Spec.Templates[i]
		}
	}
	panic("template " + name + " not found")
}
