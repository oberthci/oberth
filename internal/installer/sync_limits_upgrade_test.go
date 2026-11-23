package installer_test

import (
	"errors"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestUpgradePreservesExternalSyncLimits(t *testing.T) {
	t.Parallel()
	for _, poisoned := range []bool{false, true} {
		t.Run(boolText(poisoned), func(t *testing.T) { syncLimitsUpgradeCase(t, poisoned) })
	}
}

func syncLimitsUpgradeCase(t *testing.T, poisoned bool) {
	t.Helper()
	old := installer.ReleaseChartForTest(loadFixture(t, releaseFixture(t)))
	store := storage.Init(driver.NewMemory())
	kc := syncLimitsUpgradeClient(t)
	before := jsonOf(t, kc.info.Object)
	manifest := syncLimitsManifest(t, old)
	if poisoned {
		manifest += "\n---\n" + before + "\n"
	}
	record := &release.Release{Name: "oberth", Namespace: "oberth", Version: 1, Chart: old, Config: map[string]any{}, Manifest: manifest,
		Info: &release.Info{Status: release.StatusDeployed}, Labels: installer.RevisionLabelsForTest(installer.ChartRequest{Chart: old})}
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.30.0", Major: "1", Minor: "30"}
	port := installer.ChartForTest(&action.Configuration{Releases: store, KubeClient: kc, Capabilities: caps, Log: func(string, ...any) {}})
	target := installer.ReleaseChartForTest(loadFixture(t, releaseFor(t, "v0.2.0", 1)))
	request := installer.ChartRequest{Chart: target, Namespace: "oberth", Values: map[string]any{"runs": map[string]any{"parallel": 6}}}
	bindChartHistory(t, port, &request)
	err := port.Apply(t.Context(), request)
	if poisoned {
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || refusal.Code != "history-delete" || kc.updates != 0 {
			t.Fatal("unowned sync limits in history were not refused before upgrade", err, kc.updates)
		}
	} else {
		assertSyncLimitsUpgrade(t, store, kc, err)
		if jsonOf(t, kc.info.Object) != before {
			t.Fatal("upgrade changed the external sync limits")
		}
	}
	if len(kc.deleted) != 0 || !kc.pipelineOriginal || !kc.pipelineTarget {
		t.Fatal("upgrade deletes shared sync limits or their namespace", kc.deleted)
	}
}

func assertSyncLimitsUpgrade(t *testing.T, store *storage.Storage, kc *syncLimitsKube, err error) {
	t.Helper()
	if err != nil || kc.updates != 1 || kc.gets != 0 {
		t.Fatal("upgrade touched the external sync limits", err, kc.updates, kc.gets)
	}
	last, readErr := store.Last("oberth")
	if readErr != nil || last.Version != 2 || last.Info.Status != release.StatusDeployed {
		t.Fatal("native upgrade did not complete", readErr)
	}
}

func syncLimitsManifest(t *testing.T, ch *chart.Chart) string {
	t.Helper()
	files, err := installer.Render(ch, nil, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if strings.HasSuffix(name, ".yaml") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var manifest strings.Builder
	for _, name := range names {
		manifest.WriteString("---\n" + files[name] + "\n")
	}
	return manifest.String()
}

type syncLimitsKube struct {
	*deltaDeletionKube
	pipelineOriginal, pipelineTarget bool
}

func syncLimitsUpgradeClient(t *testing.T) *syncLimitsKube {
	t.Helper()
	k := &syncLimitsKube{deltaDeletionKube: deltaDeleteClient(t)}
	k.info.Name, k.info.Namespace = "oberth-sync-limits", "oberth-runs"
	k.live = metav1.ObjectMeta{Name: k.info.Name, Namespace: k.info.Namespace, UID: "external-map", ResourceVersion: "7"}
	k.info.Object = &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: k.live,
		Data: map[string]string{"go": "2", "another-repository": "3"}}
	return k
}

// Observe the complete inventories, then run the native update/delete algorithm
// for the external object over the existing in-memory REST transport.
func (k *syncLimitsKube) Build(reader io.Reader, validate bool) (kube.ResourceList, error) {
	decoder := kyaml.NewYAMLOrJSONDecoder(reader, 4096)
	var result kube.ResourceList
	for {
		var object struct {
			Kind     string
			Metadata metav1.ObjectMeta
		}
		err := decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		if object.Kind == "Namespace" && object.Metadata.Name == "oberth-runs" {
			if validate {
				k.pipelineTarget = true
			} else {
				k.pipelineOriginal = true
			}
		}
		if object.Kind == "ConfigMap" && object.Metadata.Name == k.info.Name && object.Metadata.Namespace == k.info.Namespace {
			result = append(result, k.info)
		}
	}
}
