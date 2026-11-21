package installer_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/kube"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
)

func TestExistingNamespacesStayUnmarkedAndSurviveUninstall(t *testing.T) {
	t.Parallel()
	f := setup(t)
	before := map[string]string{}
	for _, name := range []string{"oberth", "oberth-runs"} {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"hand-made": "yes", "pod-security.kubernetes.io/enforce": "baseline"}, Annotations: map[string]string{"purpose": "shared infrastructure"}}}
		created, err := f.client.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		before[name] = jsonOf(t, created)
	}
	apply(t, f, prepare(t, f))
	apply(t, f, prepare(t, f))
	request := f.charts.Applies[len(f.charts.Applies)-1]
	files, err := installer.Render(request.Chart, request.Values, request.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	var manifest strings.Builder
	for name, raw := range files {
		if strings.HasSuffix(name, ".yaml") {
			manifest.WriteString("---\n" + raw + "\n")
		}
	}
	nativeNamespaceUninstall(t, f, manifest.String())
	for name, old := range before {
		current, err := f.client.CoreV1().Namespaces().Get(t.Context(), name, metav1.GetOptions{})
		if err != nil || jsonOf(t, current) != old {
			t.Fatalf("namespace %s changed or was removed: %v", name, err)
		}
	}
}

func nativeNamespaceUninstall(t *testing.T, f fixture, manifest string) {
	t.Helper()
	store := storage.Init(driver.NewMemory())
	record := &release.Release{Name: "oberth", Namespace: "oberth", Version: 1, Info: &release.Info{Status: release.StatusDeployed}, Manifest: manifest}
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	client := &namespaceUninstallKube{PrintingKubeClient: &kubefake.PrintingKubeClient{Out: io.Discard}, client: f.client}
	uninstall := action.NewUninstall(&action.Configuration{Releases: store, KubeClient: client, Capabilities: chartutil.DefaultCapabilities, Log: func(string, ...any) {}})
	uninstall.DisableHooks = true
	if _, err := uninstall.Run("oberth"); err != nil {
		t.Fatal(err)
	}
}

// Native Helm selects the uninstall manifest; this transport observes Namespace deletions.
type namespaceUninstallKube struct {
	*kubefake.PrintingKubeClient
	client kubernetes.Interface
}

func (k *namespaceUninstallKube) Build(reader io.Reader, _ bool) (kube.ResourceList, error) {
	decoder := kyaml.NewYAMLOrJSONDecoder(reader, 4096)
	var result kube.ResourceList
	for {
		var object map[string]any
		err := decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		if object["kind"] == "Namespace" {
			result = append(result, &resource.Info{Name: object["metadata"].(map[string]any)["name"].(string)})
		}
	}
}
func (k *namespaceUninstallKube) Delete(resources kube.ResourceList) (*kube.Result, []error) {
	for _, object := range resources {
		if err := k.client.CoreV1().Namespaces().Delete(context.Background(), object.Name, metav1.DeleteOptions{}); err != nil {
			return nil, []error{err}
		}
	}
	return &kube.Result{}, nil
}
func (k *namespaceUninstallKube) DeleteWithPropagationPolicy(resources kube.ResourceList, _ metav1.DeletionPropagation) (*kube.Result, []error) {
	return k.Delete(resources)
}

func TestSystemNamespacesRefusedInBothRoles(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"default", "kube-system", "kube-operator"} {
		for _, role := range []string{"server", "pipeline"} {
			t.Run(role+"/"+name, func(t *testing.T) {
				f := setup(t)
				if role == "server" {
					f.choices.Namespace = name
				} else {
					f.choices.PipelineNamespace = name
				}
				_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
				if err == nil || !strings.Contains(err.Error(), "System namespaces") || !strings.Contains(err.Error(), "Choose another namespace") {
					t.Fatal(err)
				}
				assertNoInstallWrites(t, f)
			})
		}
	}
}

func TestUnmarkedPipelineHeldByRelease(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"server", "pipeline"} {
		for _, recheck := range []bool{false, true} {
			t.Run(role+map[bool]string{true: " recheck"}[recheck], func(t *testing.T) { unmarkedPipelineCase(t, role, recheck) })
		}
	}
}
func unmarkedPipelineCase(t *testing.T, role string, recheck bool) {
	f := setup(t)
	f.choices.Namespace = "second"
	if role == "server" {
		f.choices.Namespace = "jobs"
	} else {
		f.choices.PipelineNamespace = "jobs"
	}
	var plan *installer.Plan
	if recheck {
		plan = prepare(t, f)
	}
	record := &release.Release{Name: "oberth", Namespace: "first", Version: 1, Info: &release.Info{Status: release.StatusDeployed}, Config: map[string]any{"pipeline": map[string]any{"namespace": "jobs"}}}
	if err := installer.ReleaseSecretsForTest(f.client.CoreV1().Secrets("first")).Create("sh.helm.release.v1.oberth.v1", record); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	var err error
	if recheck {
		_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
	} else {
		_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
	}
	if err == nil || !strings.Contains(err.Error(), "jobs") || !strings.Contains(err.Error(), "first/oberth") {
		t.Fatal(err)
	}
	assertNoInstallWrites(t, f)
}

func TestCreatedPipelineNamespaceRemainsInRevision(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	// The persisted choice must survive even if namespace marks are later removed.
	if _, err := f.client.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "oberth-runs", Labels: map[string]string{"pod-security.kubernetes.io/enforce": "baseline"}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	apply(t, f, prepare(t, f))
	object(t, rendered(t, f.charts.Applies[1].Values), "Namespace", "oberth-runs")
}
