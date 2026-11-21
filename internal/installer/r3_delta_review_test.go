package installer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"helm.sh/helm/v3/pkg/storage"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/kube"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

type deltaRoundTrip func(*http.Request) (*http.Response, error)

func (f deltaRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Decode only the injected object. The authenticated target contains no such
// object. Its unrelated resources are omitted from this focused deletion probe.
type deltaDeletionKube struct {
	*kubefake.PrintingKubeClient
	info           *resource.Info
	deleted        []string
	updates        int
	live           metav1.ObjectMeta
	getStatus      int
	gets           int
	alterOwnership bool
	onReach        func()
	reaches        int
}

func (k *deltaDeletionKube) Build(r io.Reader, _ bool) (kube.ResourceList, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(b, []byte("review-history-injection")) {
		return kube.ResourceList{k.info}, nil
	}
	return kube.ResourceList{}, nil
}
func (k *deltaDeletionKube) Update(original, target kube.ResourceList, force bool) (*kube.Result, error) {
	k.updates++
	// Run the dependency's real update/delete algorithm over an in-memory REST
	// transport. No socket, API server, or handwritten deletion algorithm.
	return kube.New(nil).Update(original, target, force)
}
func deltaDeleteClient(t *testing.T) *deltaDeletionKube {
	t.Helper()
	k := &deltaDeletionKube{PrintingKubeClient: &kubefake.PrintingKubeClient{Out: io.Discard}}
	config := &rest.Config{Host: "https://offline.invalid", ContentConfig: rest.ContentConfig{
		GroupVersion: &schema.GroupVersion{Version: "v1"}, NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
	}, APIPath: "/api", Transport: deltaRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			k.gets++
			if k.alterOwnership && k.gets >= 2 {
				k.live.Annotations["meta.helm.sh/release-name"] = "another"
			}
		}
		if status := k.responseStatus(r.Method); status != 0 {
			return deltaStatusResponse(r, status)
		}
		metadata := k.live
		metadata.Name, metadata.Namespace = k.info.Name, k.info.Namespace
		payload, marshalErr := json.Marshal(map[string]any{"apiVersion": k.info.Mapping.GroupVersionKind.GroupVersion().String(), "kind": k.info.Mapping.GroupVersionKind.Kind, "metadata": metadata})
		if marshalErr != nil {
			return nil, marshalErr
		}
		body := string(payload)
		if r.Method == "DELETE" {
			k.deleted = append(k.deleted, r.URL.Path)
			body = `{"apiVersion":"v1","kind":"Status","status":"Success"}`
		}
		if r.Method != "GET" && r.Method != "DELETE" {
			return nil, fmt.Errorf("unexpected method %s", r.Method)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	client, err := rest.RESTClientFor(config)
	if err != nil {
		t.Fatal(err)
	}
	k.info = &resource.Info{Client: client, Namespace: "oberth", Name: "review-history-injection", Object: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "review-history-injection", Namespace: "oberth"}}, Mapping: &meta.RESTMapping{
		Resource: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, GroupVersionKind: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Scope: meta.RESTScopeNamespace,
	}}
	return k
}
func TestDeltaHistoryCannotDeleteUnrelatedObject(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"clean", "upgrade", "repair", "rollback"} {
		t.Run(mode, func(t *testing.T) { deltaHistoryDeletionCase(t, mode) })
	}
}

func deltaReadProfile(t *testing.T, f fixture) []byte {
	t.Helper()
	b, err := os.ReadFile(f.dir + "/installation.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func deltaWriteProfile(t *testing.T, f fixture, b []byte) {
	t.Helper()
	if err := os.WriteFile(f.dir+"/installation.json", b, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestDeltaLocalCannotReplaceVerifiedAfterRecordChange(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"replayed-local-profile", "edited-kind"} {
		t.Run(shape, func(t *testing.T) {
			f := setup(t)
			f.release = localVersion(t, "v0.2.0", 2)
			apply(t, f, prepare(t, f))
			localProfile := deltaReadProfile(t, f)
			f.release = loadFixture(t, releaseFor(t, "v0.2.0", 2))
			apply(t, f, prepare(t, f))
			if shape == "replayed-local-profile" {
				deltaWriteProfile(t, f, localProfile)
			} else {
				var profile map[string]any
				if err := json.Unmarshal(deltaReadProfile(t, f), &profile); err != nil {
					t.Fatal(err)
				}
				profile["release_kind"] = "local"
				b, _ := json.Marshal(profile)
				deltaWriteProfile(t, f, b)
			}
			f.release = localVersion(t, "v0.2.0", 2)
			p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if err != nil {
				t.Log("refused:", err)
				return
			}
			defer p.Close()
			_, err = f.engine.Apply(t.Context(), p, p.Digest(), nil)
			if err != nil {
				t.Fatalf("local replacement reached preview before refusal: %v", err)
			}
			t.Fatal("local chart applied after a verified release without removing the installation")
		})
	}
}
func TestDeltaStaleProfileCannotLowerSchema(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.release = loadFixture(t, releaseFor(t, "v0.1.0", 1))
	apply(t, f, prepare(t, f))
	oldProfile := deltaReadProfile(t, f)
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 2))
	deltaUpgrade(t, f, 1)
	// An older saved profile survives on another operator workstation, or is
	// restored from a local backup. Cluster history continues to name v0.2.0.
	deltaWriteProfile(t, f, oldProfile)
	f.release = loadFixture(t, releaseFor(t, "v0.1.0", 1))
	p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if err != nil {
		t.Log("refused:", err)
		return
	}
	defer p.Close()
	_, err = f.engine.Apply(t.Context(), p, p.Digest(), nil)
	if err != nil {
		t.Fatalf("lower schema reached preview before refusal: %v", err)
	}
	t.Fatal("schema 2 -> 1 applied using stale profile despite deployed v0.2.0 history")
}
func TestDeltaFailedFirstInstallUnknownSchemaRefuses(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 2))
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return fmt.Errorf("interrupted after server start") }
	p := prepare(t, f)
	if _, err := f.engine.Apply(t.Context(), p, p.Digest(), nil); err == nil {
		t.Fatal("setup did not interrupt")
	}
	f.charts.History.Latest.Status = "failed"
	f.charts.History.Deployed = installer.Revision{}
	if err := os.Remove(f.dir + "/installation.json"); err != nil {
		t.Fatal(err)
	}
	f.release = loadFixture(t, releaseFor(t, "v0.1.0", 1))
	p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if err == nil {
		p.Close()
		t.Fatal("failed first revision with missing schema/profile accepted a lower signed schema")
	}
}

func TestDeltaLocalProfileShapesRefuse(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"missing", "empty", "directory", "outside-symlink", "missing-kind", "unknown-kind"} {
		t.Run(shape, func(t *testing.T) { deltaProfileShapeCase(t, shape) })
	}
}

// Version changes use maintenance with an explicit live schema under the R3 ruling.
func deltaUpgrade(t *testing.T, f fixture, schema int) {
	t.Helper()
	m := installer.Maintenance{Engine: f.engine, ActiveRuns: func(context.Context, string) (int, error) { return 0, nil }, Options: stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: schema}}
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	if _, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
}

func deltaHistoryBytes(t *testing.T, store *storage.Storage) []byte {
	t.Helper()
	records, err := store.History("oberth")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Version < records[j].Version })
	var values []any
	for _, record := range records {
		values = append(values, map[string]any{"record": record, "labels": record.Labels})
	}
	body, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func deltaHistoryDeletionCase(t *testing.T, mode string) {
	t.Helper()

	store, request := reviewHistoryFixture(t)
	old, err := store.Get("oberth", 2)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "clean" {
		poisonHistoryTarget(old, "both")
	}
	if err = store.Update(old); err != nil {
		t.Fatal(err)
	}
	if mode == "upgrade" {
		if _, err = store.Delete("oberth", 3); err != nil {
			t.Fatal(err)
		}
		request.Revision, request.Recovery = 2, nil
	}
	if mode == "rollback" {
		request.Recovery = nil
		request.RollbackID = "reviewed"
	}
	request.Chart = installer.ReleaseChartForTest(loadFixture(t, releaseFixture(t)))
	kc := deltaDeleteClient(t)
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.30.0", Major: "1", Minor: "30"}
	port := installer.ChartForTest(&action.Configuration{Releases: store, KubeClient: kc, Capabilities: caps, Log: func(string, ...any) {}})
	bindChartHistory(t, port, &request)
	before := deltaHistoryBytes(t, store)
	err = port.Apply(t.Context(), request)
	if mode == "clean" {
		if err != nil || kc.updates != 1 {
			t.Fatal("clean native upgrade was not applied", err)
		}
	} else {
		deltaAssertDeletionRefused(t, store, before, err)
	}
	if len(kc.deleted) != 0 {
		t.Fatalf("unsigned historical manifest caused native DELETE %v (apply error=%v)", kc.deleted, err)
	}
	if err != nil {
		t.Log("refused without deletion:", err)
	}

}

func deltaProfileShapeCase(t *testing.T, shape string) {
	t.Helper()

	f := setup(t)
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 2))
	apply(t, f, prepare(t, f))
	deltaChangeProfile(t, f, shape)
	local := localVersion(t, "v0.2.0", 2)
	before := len(f.charts.Previews)
	p, err := f.engine.Prepare(t.Context(), local, f.choices)
	if err == nil {
		p.Close()
		t.Fatal("unsafe profile accepted")
	}
	if len(f.charts.Previews) != before {
		t.Fatal("unsafe profile previewed")
	}

}

func deltaChangeProfile(t *testing.T, f fixture, shape string) {
	t.Helper()
	path := f.dir + "/installation.json"
	saved := deltaReadProfile(t, f)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	switch shape {
	case "empty":
		deltaWriteProfile(t, f, nil)
	case "directory":
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	case "outside-symlink":
		outside := t.TempDir() + "/profile"
		if err := os.WriteFile(outside, saved, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
	case "missing-kind", "unknown-kind":
		var p map[string]any
		if err := json.Unmarshal(saved, &p); err != nil {
			t.Fatal(err)
		}
		delete(p, "release_kind")
		if shape == "unknown-kind" {
			p["release_kind"] = "other"
		}
		saved, _ = json.Marshal(p)
		deltaWriteProfile(t, f, saved)
	}

}

func deltaAssertDeletionRefused(t *testing.T, store *storage.Storage, before []byte, err error) {
	t.Helper()
	var problem *installer.Problem
	if !errors.As(err, &problem) || problem.Code != "history-delete" || !strings.Contains(err.Error(), "review-history-injection") || !bytes.Equal(before, deltaHistoryBytes(t, store)) {
		t.Fatal("deletion preflight did not refuse before history writes", err)
	}
}

func (k *deltaDeletionKube) IsReachable() error {
	k.reaches++
	if k.onReach != nil {
		k.onReach()
	}
	return nil
}

func deltaStatusResponse(r *http.Request, code int) (*http.Response, error) {
	reason := metav1.StatusReasonUnknown
	if code == http.StatusNotFound {
		reason = metav1.StatusReasonNotFound
	}
	raw, err := json.Marshal(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: reason, Code: int32(code), Message: http.StatusText(code)})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
}

func (k *deltaDeletionKube) responseStatus(method string) int {
	if method == "GET" || method == "DELETE" && k.getStatus == http.StatusNotFound {
		return k.getStatus
	}
	return 0
}
