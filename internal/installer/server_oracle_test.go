package installer_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sigs.k8s.io/yaml"
	"testing"

	"github.com/argoproj/argo-workflows/v4/config"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob/argojobtest"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

func TestRenderedNativeExecutionOracle(t *testing.T) {
	t.Parallel()
	objects := rendered(t, map[string]any{"pipeline": map[string]any{"namespace": "jobs"}})
	wantRole, wantBinding := argojobtest.TaskResultAccess("jobs")
	var role rbacv1.Role
	var binding rbacv1.RoleBinding
	var policy admissionv1.ValidatingAdmissionPolicy
	for _, target := range []struct {
		kind  string
		value any
	}{{"Role", &role}, {"RoleBinding", &binding}, {"ValidatingAdmissionPolicy", &policy}} {
		if err := json.Unmarshal([]byte(jsonOf(t, object(t, objects, target.kind, "oberth-task-results"))), target.value); err != nil {
			t.Fatal(err)
		}
	}
	if role.Namespace != wantRole.Namespace || !reflect.DeepEqual(role.Rules, wantRole.Rules) || binding.Namespace != wantBinding.Namespace || !reflect.DeepEqual(binding.Subjects, wantBinding.Subjects) || binding.RoleRef != wantBinding.RoleRef {
		t.Fatal("rendered task-result access differs from native execution oracle")
	}
	if !reflect.DeepEqual(policy.Spec, argojobtest.TaskResultPolicy("jobs").Spec) {
		t.Fatal("rendered task-result policy differs from native execution oracle")
	}
}

func TestRenderedPluginAndDefaultsUsePinnedController(t *testing.T) {
	t.Parallel()
	values := map[string]any{
		"sourcePluginImageDigest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"argoNative":              map[string]any{"controller": map[string]any{"workflowDefaults": map[string]any{"spec": map[string]any{"activeDeadlineSeconds": 1800}}}},
	}
	objects := rendered(t, values)
	data := object(t, objects, "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
	cm := &corev1.ConfigMap{Data: map[string]string{}}
	for k, v := range data {
		cm.Data[k] = v.(string)
	}
	native, err := config.NewController("oberth", "oberth-controller-config", nil).Parse(cm)
	if err != nil {
		t.Fatal("controller cannot load installed registration", err)
	}
	drivers, err := native.GetArtifactDrivers([]wf.ArtifactPluginName{"oberth"})
	if err != nil || len(drivers) != 1 || drivers[0].Image != "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth-source-plugin@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" {
		t.Fatal("source plugin is not registered with the pinned controller", drivers, err)
	}
	if !reflect.DeepEqual(native.Images[drivers[0].Image].Entrypoint, []string{"/usr/local/bin/oberth-source-plugin"}) {
		t.Fatal("controller cannot start the artifact socket entrypoint")
	}
	environment := map[string]string{}
	for _, variable := range native.Executor.Env {
		environment[variable.Name] = variable.Value
	}
	if environment["OBERTH_SOURCE_ENDPOINT"] != "https://oberth-source.oberth.svc:8445" || environment["OBERTH_SOURCE_SERVER_NAME"] != "oberth.oberth.svc" {
		t.Fatal("registered artifact container cannot locate the source endpoint")
	}
	assertControllerDefaultsSnapshot(t, objects, data)
}

func assertControllerDefaultsSnapshot(t *testing.T, objects []map[string]any, data map[string]any) {
	t.Helper()
	raw, err := yaml.YAMLToJSON([]byte(data["workflowDefaults"].(string)))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	cfg, err := decodeChartServer([]byte(jsonOf(t, serverJSON(t, objects))))
	if err != nil || hex.EncodeToString(digest[:]) != cfg.Runtime.Defaults.Digest || string(raw) != string(cfg.Runtime.Defaults.WorkflowDefaults) {
		t.Fatal("server and controller read different defaults", err)
	}
}

func TestRenderedArgoNamespaceFollowsInstalledController(t *testing.T) {
	t.Parallel()
	objects := renderedIn(t, map[string]any{"argoNative": map[string]any{"namespaceOverride": "controller-ns"}}, "frontdoor")
	config := serverJSON(t, objects)
	if config["namespace"] != "frontdoor" || config["argo_namespace"] != "controller-ns" {
		t.Fatal("server config does not name installed controller namespace", config)
	}
	for _, target := range []struct{ kind, name string }{{"ServiceAccount", "oberth-controller"}, {"ConfigMap", "oberth-controller-config"}, {"Role", "oberth-controller-config"}, {"RoleBinding", "oberth-controller-config"}} {
		if object(t, objects, target.kind, target.name)["metadata"].(map[string]any)["namespace"] != "controller-ns" {
			t.Fatal("installed controller configuration or access is in another namespace", target)
		}
	}
	var binding rbacv1.RoleBinding
	if err := json.Unmarshal([]byte(jsonOf(t, object(t, objects, "RoleBinding", "oberth-controller"))), &binding); err != nil {
		t.Fatal(err)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Namespace != "controller-ns" {
		t.Fatal("pipeline access does not reach installed controller account", binding)
	}
}
