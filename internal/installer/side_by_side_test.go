package installer_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	installfake "github.com/oberthci/oberth/internal/installer/fake"
	corev1 "k8s.io/api/core/v1"
	definitions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

func pinnedCRDs(t *testing.T, f fixture) []definitions.CustomResourceDefinition {
	t.Helper()
	var result []definitions.CustomResourceDefinition
	for _, dep := range installer.ReleaseChartForTest(f.release).Dependencies() {
		for _, file := range dep.Files {
			if !strings.HasPrefix(file.Name, "files/crds/minimal/") {
				continue
			}
			var crd definitions.CustomResourceDefinition
			if err := yaml.Unmarshal(file.Data, &crd); err != nil {
				t.Fatal(err)
			}
			crd.Status.StoredVersions = []string{"v1alpha1"}
			result = append(result, crd)
		}
	}
	if len(result) != 8 {
		t.Fatal("fixture needs eight pinned definitions", len(result))
	}
	return result
}

func definitionFixture(t *testing.T, f fixture, owner string) *installfake.Definitions {
	t.Helper()
	client := &installfake.Definitions{Objects: map[string]*definitions.CustomResourceDefinition{}}
	for _, crd := range pinnedCRDs(t, f) {
		if owner != "" {
			crd.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
			crd.Annotations["meta.helm.sh/release-name"] = "oberth"
			crd.Annotations["meta.helm.sh/release-namespace"] = owner
		}
		client.Objects[crd.Name] = crd.DeepCopy()
	}
	f.engine.Definitions = client
	return client
}

func TestWorkflowDefinitionOwnershipCases(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"absent", "oberth", "control"} {
		for _, reader := range []string{"fake", "typed"} {
			t.Run(owner+"/"+reader, func(t *testing.T) { definitionOwnershipCase(t, owner, reader) })
		}
	}
}

func definitionOwnershipCase(t *testing.T, owner, readerKind string) {
	f := setup(t)
	var reader *installfake.Definitions
	if owner != "absent" {
		reader = definitionFixture(t, f, owner)
	}
	if readerKind == "typed" {
		f.engine.Definitions = typedDefinitionReader(t, reader)
	}
	p := prepare(t, f)
	rendered, err := installer.Render(installer.ReleaseChartForTest(f.release), f.charts.Previews[0].Values, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	count := strings.Count(jsonOf(t, rendered), "kind: CustomResourceDefinition")
	want := 8
	if owner == "control" {
		want = 0
		if !strings.Contains(strings.Join(p.Review(), "\n"), "Workflow definitions: reuse; owner control/oberth") {
			t.Fatal(p.Review())
		}
	}
	if count != want {
		t.Fatal("wrong definition render count", count, want)
	}
	if reader != nil && readerKind == "fake" {
		assertDefinitionReads(t, reader)
	}
}

func assertDefinitionReads(t *testing.T, reader *installfake.Definitions) {
	t.Helper()
	if len(reader.Reads) != 8 {
		t.Fatal("not all definitions inspected", reader.Reads)
	}

}

func TestWorkflowDefinitionMixedOwnership(t *testing.T) {
	t.Parallel()
	f := setup(t)
	reader := definitionFixture(t, f, "control")
	api := reader
	delete(api.Objects, "workflows.argoproj.io")
	_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if err == nil || !strings.Contains(err.Error(), "Missing definitions: workflows.argoproj.io") {
		t.Fatal(err)
	}
	assertNoInstallWrites(t, f)
}

func TestWorkflowDefinitionIncompatibilityRefusesBeforeWrites(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"served", "storage", "stored", "field", "spec field", "field type", "scope", "read failure"} {
		t.Run(change, func(t *testing.T) {
			f := setup(t)
			reader := definitionFixture(t, f, "control")
			api := reader
			crd, err := api.Get(t.Context(), "workflows.argoproj.io", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			corruptDefinition(crd, change)
			if change == "read failure" {
				reader.Err = errors.New("definition read denied")
			}
			api.Objects[crd.Name] = crd

			_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
			if err == nil {
				t.Fatal("incompatible definition accepted", change)
			}
			if change != "read failure" && !strings.Contains(err.Error(), crd.Name) {
				t.Fatal("refusal did not identify definition", err)
			}
			assertNoInstallWrites(t, f)
		})
	}
}

func assertNoInstallWrites(t *testing.T, f fixture) {
	t.Helper()
	for _, action := range f.client.Actions() {
		if !readOnly(action) {
			t.Fatal("preflight wrote", action)
		}
	}
	files, err := os.ReadDir(f.dir)
	if err != nil || len(files) != 0 || len(f.charts.Applies) != 0 {
		t.Fatal("preflight saved credentials or applied", files, err)
	}
}

func TestDefinitionChangeAfterReviewRefuses(t *testing.T) {
	t.Parallel()
	f := setup(t)
	reader := definitionFixture(t, f, "control")
	p := prepare(t, f)
	delete(reader.Objects, "workflows.argoproj.io")
	if _, err := f.engine.Apply(t.Context(), p, p.Digest(), nil); err == nil {
		t.Fatal("definition change after review accepted")
	}
	assertNoInstallWrites(t, f)
}

func foreignService(t *testing.T, f fixture, port int32) {
	t.Helper()
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "front", Namespace: "existing"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 443, NodePort: port}}}}
	if _, err := f.client.CoreV1().Services(service.Namespace).Create(t.Context(), service, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
}

func TestNodePortPreflight(t *testing.T) {
	t.Parallel()
	for _, port := range []int32{30443, 30022} {
		f := setup(t)
		foreignService(t, f, port)
		_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "--") || len(refusal.Reason) > 80 {
			t.Fatal("port conflict lacks concise remedy", err)
		}
		assertNoInstallWrites(t, f)
	}
}

func TestNodePortPermissionScopeAndFailure(t *testing.T) {
	t.Parallel()
	for _, fallbackFails := range []bool{false, true} {
		f := setup(t)
		f.client.PrependReactor("list", "services", func(action ktesting.Action) (bool, runtime.Object, error) {
			if action.GetNamespace() == "" || fallbackFails {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", errors.New("list denied"))
			}
			return false, nil, nil
		})
		p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
		if fallbackFails {
			if err == nil {
				t.Fatal("unknown service inventory accepted")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if !strings.Contains(strings.Join(p.Review(), "\n"), "installation namespaces only") {
				t.Fatal("unread scope claimed", p.Review())
			}
		}
	}
}

func TestNodePortCollisionAfterReviewRefuses(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := prepare(t, f)
	foreignService(t, f, 30443)
	if _, err := f.engine.Apply(t.Context(), p, p.Digest(), nil); err == nil {
		t.Fatal("port collision after review accepted")
	}
	assertNoInstallWrites(t, f)
}

func TestOwnedNodePortsAreReused(t *testing.T) {
	t.Parallel()
	f := setup(t)
	foreignService(t, f, 30443)
	service, err := f.client.CoreV1().Services("existing").Get(t.Context(), "front", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	service.Namespace = "oberth"
	service.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
	service.Annotations = map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "oberth"}
	if err = f.client.CoreV1().Services("existing").Delete(t.Context(), "front", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.client.CoreV1().Services("oberth").Create(t.Context(), service, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	prepare(t, f)
}

func TestTwoInstallationClusterNamesAndNamespaceDerivations(t *testing.T) {
	t.Parallel()
	first := clusterNames(rendered(t, nil))
	second := renderedIn(t, nil, "second")
	for name := range clusterNames(second) {
		if first[name] && !strings.HasPrefix(name, "CustomResourceDefinition/") {
			t.Fatal("cluster name collision", name)
		}
	}
	assertNoDefaultNamespace(t, second)
	cm := object(t, second, "ConfigMap", "oberth-server")["data"].(map[string]any)["server.json"].(string)
	var config map[string]any
	if err := json.Unmarshal([]byte(cm), &config); err != nil {
		t.Fatal(err)
	}
	if config["namespace"] != "second" || config["pipeline_namespace"] != "second-runs" || config["argo_namespace"] != "second" {
		t.Fatal(config)
	}
	controller := object(t, second, "Deployment", "oberth-controller-workflow-controller")
	if !strings.Contains(jsonOf(t, controller), "--namespaced") || !strings.Contains(jsonOf(t, controller), "--managed-namespace=second-runs") {
		t.Fatal("controller watch scope wrong", controller)
	}
}

func clusterNames(objects []map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, o := range objects {
		meta := o["metadata"].(map[string]any)
		if meta["namespace"] == nil {
			names[o["kind"].(string)+"/"+meta["name"].(string)] = true
		}
	}
	return names
}

func TestWorkflowDefinitionCompatibleSchemaVariation(t *testing.T) {
	t.Parallel()
	f := setup(t)
	reader := definitionFixture(t, f, "control")
	api := reader
	crd, err := api.Get(t.Context(), "workflows.argoproj.io", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	crd.Labels["helm.sh/chart"] = "workflow-9.8.7"
	crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "Compatible optional documentation"
	api.Objects[crd.Name] = crd
	prepare(t, f)
}

func corruptDefinition(crd *definitions.CustomResourceDefinition, change string) {
	switch change {
	case "served":
		crd.Spec.Versions[0].Served = false
	case "storage":
		crd.Spec.Versions[0].Storage = false
	case "stored":
		crd.Status.StoredVersions = []string{"v9"}
	case "field":
		delete(crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties, "spec")
	case "spec field":
		props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties
		props["spec"] = definitions.JSONSchemaProps{Type: "object"}
	case "field type":
		props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties
		props["spec"] = definitions.JSONSchemaProps{Type: "string"}
	case "scope":
		crd.Spec.Scope = definitions.ClusterScoped
	}
}

func assertNoDefaultNamespace(t *testing.T, second []map[string]any) {
	t.Helper()
	for _, o := range second {
		raw := jsonOf(t, o)
		for _, forbidden := range []string{`.oberth.svc`, `"namespace":"oberth"`, `"namespace":"oberth-runs"`, `"kubernetes.io/metadata.name":"oberth"`, `--managed-namespace=oberth-runs`, `system:serviceaccount:oberth-runs:`, `system:serviceaccounts:oberth-runs`} {
			if strings.Contains(raw, forbidden) {
				t.Fatal("default namespace leaked", forbidden, raw)
			}
		}
	}
}

func TestOwnedDefinitionsUseChartUpgrade(t *testing.T) {
	t.Parallel()
	f := setup(t)
	reader := definitionFixture(t, f, "oberth")
	reader.Objects["workflows.argoproj.io"].Spec.Versions[0].Served = false
	p := prepare(t, f)
	if strings.Contains(strings.Join(p.Review(), "\n"), "Workflow definitions: reuse") {
		t.Fatal("owned definition treated as shared", p.Review())
	}
	files, err := installer.Render(installer.ReleaseChartForTest(f.release), f.charts.Previews[0].Values, "oberth")
	if err != nil || strings.Count(jsonOf(t, files), "kind: CustomResourceDefinition") != 8 {
		t.Fatal("owned definition upgrade suppressed", err)
	}
}
