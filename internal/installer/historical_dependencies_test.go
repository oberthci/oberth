package installer_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/storage"
	clientfake "k8s.io/client-go/kubernetes/fake"
)

func serializedDependencyFixture(t *testing.T) (installer.ChartLibrary, *storage.Storage) {
	t.Helper()
	chart, err := loader.LoadFiles([]*loader.BufferedFile{
		{Name: "Chart.yaml", Data: []byte("apiVersion: v2\nname: parent\nversion: 1.0.0\nappVersion: 1.0.0\ndependencies:\n- name: child\n  version: 1.0.0\n")},
		{Name: "values.yaml", Data: []byte("{}")},
		{Name: "charts/child/Chart.yaml", Data: []byte("apiVersion: v2\nname: child\nversion: 1.0.0\n")},
		{Name: "charts/child/templates/config.yaml", Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: child\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := storage.Init(installer.ReleaseSecretsForTest(clientfake.NewClientset().CoreV1().Secrets("oberth")))
	port := installer.ChartForTest(&action.Configuration{
		Releases:     store,
		KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities,
		Log:          func(string, ...any) {},
	})
	if err = port.Apply(t.Context(), installer.ChartRequest{Chart: chart, Namespace: "oberth", Values: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	return port, store
}
func TestSerializedHistoryDependencyReapplyRefusesBeforeWrites(t *testing.T) {
	t.Parallel()
	port, store := serializedDependencyFixture(t)
	old, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	if len(old.Chart.Dependencies()) != 0 || len(old.Chart.Metadata.Dependencies) != 1 {
		t.Fatal("fixture did not model serialized history")
	}
	before, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	err = port.Apply(t.Context(), installer.ChartRequest{Chart: old.Chart, Namespace: "oberth", Revision: 1, Values: old.Config, RollbackID: "owned"})
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "chart-files" {
		t.Fatal("historical chart was reapplied without its dependency files", err)
	}
	after, e := store.Last("oberth")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(after)
	if e != nil || !bytes.Equal(before, raw) {
		t.Fatal("refusal changed serialized history", err, e)
	}
}
