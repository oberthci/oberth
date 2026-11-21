package installer_test

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/kube"
)

// Build both fixture objects so the native action exercises real hook cleanup
// separately from Update's original list. REST deletes use the existing transport.
type foreignHookClient struct {
	*deltaDeletionKube
	cleanup, created []string
}

func (k *foreignHookClient) Build(r io.Reader, validate bool) (kube.ResourceList, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(data, []byte("name: reconcile-hook")) {
		info := *k.info
		info.Name = "reconcile-hook"
		return kube.ResourceList{&info}, nil
	}
	return k.deltaDeletionKube.Build(bytes.NewReader(data), validate)
}
func (k *foreignHookClient) Delete(items kube.ResourceList) (*kube.Result, []error) {
	for _, item := range items {
		k.cleanup = append(k.cleanup, item.Name)
	}
	return kube.New(nil).Delete(items)
}
func (k *foreignHookClient) Create(items kube.ResourceList) (*kube.Result, error) {
	for _, item := range items {
		k.created = append(k.created, item.Name)
	}
	return k.PrintingKubeClient.Create(items)
}

func TestForeignHookCleanupKeepsTargetObject(t *testing.T) {
	t.Parallel()
	for _, foreign := range []bool{false, true} {
		t.Run(boolText(foreign), func(t *testing.T) { foreignHookCase(t, foreign) })
	}
}

func foreignHookCase(t *testing.T, foreign bool) {
	f, store, source := foreignInstallFixture(t, true)
	revision := 1
	if foreign {
		revision = appendForeignRevisions(t, store)[1]
	}
	old, err := store.Get("oberth", 1)
	if err != nil {
		t.Fatal(err)
	}
	ch := &chart.Chart{Metadata: &chart.Metadata{Name: "oberth", APIVersion: "v2", Version: "1.0.0", AppVersion: f.release.Version()}, Values: map[string]any{}}
	ch.Templates = append(ch.Templates,
		&chart.File{Name: "templates/retained.yaml", Data: []byte(old.Manifest)},
		&chart.File{Name: "templates/reconcile-hook.yaml", Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: reconcile-hook\n  namespace: oberth\n  annotations:\n    helm.sh/hook: pre-upgrade\n")})
	client := &foreignHookClient{deltaDeletionKube: source}
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.30.0", Major: "1", Minor: "30"}
	port := installer.ChartForTest(&action.Configuration{Releases: store, KubeClient: client, Capabilities: caps, Log: func(string, ...any) {}})
	request := installer.ChartRequest{Chart: ch, Namespace: "oberth", Revision: revision, Values: map[string]any{}}
	bindChartHistory(t, port, &request)
	before := deltaHistoryBytes(t, store)
	if err = port.Preview(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, deltaHistoryBytes(t, store)) || len(client.cleanup) != 0 {
		t.Fatal("preview executed hook cleanup")
	}
	if err = port.Apply(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	assertForeignHook(t, client, source)
}

func assertForeignHook(t *testing.T, client *foreignHookClient, source *deltaDeletionKube) {
	t.Helper()
	if !reflect.DeepEqual(client.cleanup, []string{"reconcile-hook"}) || !reflect.DeepEqual(client.created, []string{"reconcile-hook"}) {
		t.Fatal("hook cleanup/build used ordinary objects", client.cleanup, client.created)
	}
	for _, path := range source.deleted {
		if !strings.HasSuffix(path, "/reconcile-hook") {
			t.Fatal("hook deleted retained object", path)
		}
	}
	if len(source.deleted) != 1 || source.updates != 1 {
		t.Fatal("native hook deletion or ordinary update skipped", source.deleted, source.updates)
	}
}
