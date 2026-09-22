package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestSubstituteOrgReplacesThePlaceholderInCommandArgsAndEnv(t *testing.T) {
	template := &wfv1.Template{
		Container: &corev1.Container{
			Command: []string{"/run/oberth/bin/oberth"},
			Args:    []string{"secretstore", "exec", "--path=oberth/upstream/${org}/typesafe-api-key", "--", "sh"},
			Env:     []corev1.EnvVar{{Name: "PATH_HINT", Value: "oberth/upstream/${org}/typesafe-api-key"}},
		},
	}
	SubstituteOrg(template, "transferz")
	if got := strings.Join(template.Container.Args, " "); strings.Contains(got, "${org}") || !strings.Contains(got, "transferz") {
		t.Fatalf("args = %q", got)
	}
	if template.Container.Env[0].Value != "oberth/upstream/transferz/typesafe-api-key" {
		t.Fatalf("env = %q", template.Container.Env[0].Value)
	}
}

func TestSubstituteOrgLeavesTheDocumentAloneWithoutAnOrg(t *testing.T) {
	template := &wfv1.Template{Container: &corev1.Container{Args: []string{"--path=oberth/upstream/${org}/x"}}}
	SubstituteOrg(template, "")
	if template.Container.Args[0] != "--path=oberth/upstream/${org}/x" {
		t.Fatalf("args = %q", template.Container.Args[0])
	}
	SubstituteOrg(nil, "transferz")
}

func TestSubstituteOrgCoversAScriptTemplate(t *testing.T) {
	template := &wfv1.Template{Script: &wfv1.ScriptTemplate{Source: "echo oberth/upstream/${org}/x"}}
	SubstituteOrg(template, "nikitzu")
	if template.Script.Source != "echo oberth/upstream/nikitzu/x" {
		t.Fatalf("source = %q", template.Script.Source)
	}
}
