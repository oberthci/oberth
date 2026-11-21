package installer_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/argoproj/argo-workflows/v4/config"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func TestControllerResourceVerbsInPipelineNamespace(t *testing.T) {
	t.Parallel()
	objects := rendered(t, map[string]any{"pipeline": map[string]any{"namespace": "jobs"}})
	var role rbacv1.Role
	if err := yaml.Unmarshal([]byte(jsonOf(t, object(t, objects, "Role", "oberth-controller"))), &role); err != nil {
		t.Fatal(err)
	}
	if role.Namespace != "jobs" {
		t.Fatal("controller role escaped pipeline namespace", role.Namespace)
	}
	want := map[string][]string{}
	add := func(group string, resources []string, verbs string) {
		for _, r := range resources {
			want[group+"/"+r] = splitVerbs(verbs)
		}
	}
	add("", []string{"pods"}, "create delete get list patch update watch")
	add("", []string{"pods/exec"}, "create")
	add("", []string{"configmaps"}, "get list watch")
	add("", []string{"serviceaccounts"}, "get list")
	add("", []string{"events"}, "create patch")
	add("", []string{"persistentvolumeclaims", "persistentvolumeclaims/finalizers"}, "create delete get update")
	add("policy", []string{"poddisruptionbudgets"}, "create delete get")
	add("argoproj.io", []string{"workflows", "workflows/finalizers", "workflowtasksets", "workflowtasksets/finalizers", "workflowtasksets/status", "workflowartifactgctasks"}, "create delete get list patch update watch")
	add("argoproj.io", []string{"workflowtemplates", "workflowtemplates/finalizers"}, "get list watch")
	add("argoproj.io", []string{"workflowtaskresults", "workflowtaskresults/finalizers"}, "deletecollection list watch")
	add("argoproj.io", []string{"cronworkflows", "cronworkflows/finalizers"}, "delete get list patch update watch")
	got := map[string][]string{}
	for _, rule := range role.Rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				verbs := append([]string(nil), rule.Verbs...)
				sort.Strings(verbs)
				got[group+"/"+resource] = verbs
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("controller permissions differ\ngot: %v\nwant: %v", got, want)
	}
}
func splitVerbs(s string) []string { return strings.Fields(s) }

func TestRenderedControllerConfigIsReadable(t *testing.T) {
	t.Parallel()
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(jsonOf(t, object(t, rendered(t, nil), "ConfigMap", "oberth-controller-config"))), &cm); err != nil {
		t.Fatal(err)
	}
	// Defaults merged into the spec would stop scheduled children from being recorded.
	if _, present := cm.Data["workflowDefaults"]; present {
		t.Fatal("default controller configuration sets workflowDefaults")
	}
	got, err := config.NewController(cm.Namespace, cm.Name, nil).Parse(&cm)
	if err != nil {
		t.Fatal("rendered native config is unusable", err)
	}
	if got.WorkflowDefaults != nil && !reflect.DeepEqual(got.WorkflowDefaults.Spec, wf.WorkflowSpec{}) {
		t.Fatal("rendered native empty defaults are unusable", got, err)
	}
}

func TestDefaultChartUsesNativeSchemaGate(t *testing.T) {
	t.Parallel()
	ch, err := loader.Load(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	if err = chartutil.ProcessDependencies(ch, nil); err != nil {
		t.Fatal(err)
	}
	values, err := chartutil.ToRenderValuesWithSchemaValidation(ch, nil, chartutil.ReleaseOptions{Name: "oberth", Namespace: "oberth", IsInstall: true}, chartutil.DefaultCapabilities, false)
	if err != nil {
		t.Fatal("standard schema gate rejects defaults", err)
	}
	if _, err = engine.Render(ch, values); err != nil {
		t.Fatal("standard renderer rejects defaults", err)
	}
	if _, err = installer.Render(ch, map[string]any{"unexpected": true}, "oberth"); err == nil {
		t.Fatal("parent schema is open")
	}
}
