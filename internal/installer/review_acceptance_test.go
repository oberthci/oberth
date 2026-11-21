package installer_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/kube"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	clientfake "k8s.io/client-go/kubernetes/fake"
)

// Fail-closed regressions: failing assertions are evidence, never expected success.
func TestReviewSchemaDowngradeRefusesBeforeChanges(t *testing.T) {
	t.Parallel()
	for _, noBackup := range []bool{false, true} {
		t.Run(boolText(noBackup), func(t *testing.T) { reviewSchemaDowngrade(t, noBackup) })
	}
}
func reviewSchemaDowngrade(t *testing.T, noBackup bool) {
	t.Helper()
	f, m := maintenanceFixture(t)
	f.charts.History.Deployed.Version = "v0.2.0"
	f.charts.History.Deployed.Values["targetSchema"] = 2
	f.charts.History.Latest = f.charts.History.Deployed
	m.Options.Schema = 2
	report := stateport.BackupReport{Schema: 2, AuditHead: stateport.AuditHead{ID: 7, SHA256: strings.Repeat("a", 64)}, Integrity: true, ForeignKeys: true}
	source, err := statefake.New(m.Options.Protocol, report, []byte("review schema two database"))
	if err != nil {
		t.Fatal(err)
	}
	m.Source = source
	old := loadFixture(t, releaseFor(t, "v0.1.0", 1))
	p, err := m.PlanUpgrade(t.Context(), old, f.choices, installer.UpgradeOptions{NoBackup: noBackup})
	if err != nil {
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || refusal.Code != "schema-downgrade" {
			t.Fatal("wrong downgrade refusal", err)
		}
		if len(f.charts.Applies) != 1 {
			t.Fatal("downgrade refusal changed cluster or data")
		}
		if _, statErr := os.Stat(f.dir + "/rollback.json"); !os.IsNotExist(statErr) {
			t.Fatal("downgrade refusal wrote a recovery record", statErr)
		}
		return
	}
	defer p.Plan.Close()
	_, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil)
	if err != nil {
		t.Fatalf("downgrade was not refused during planning: %v", err)
	}
	t.Fatalf("schema 2 -> 1 applied: version=%s database=%v", f.charts.History.Latest.Version, f.charts.History.Latest.Values["database"])
}

func TestReviewLocalChartCannotUpgrade(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	f.charts.History.Deployed.Version = "v0.2.0"
	setInstalledMetadata(t, f, "verified", 2)
	f.charts.History.Deployed.Kind = "verified"
	f.charts.History.Verified = true
	f.charts.History.Latest = f.charts.History.Deployed
	f.release = localVersion(t, "v0.2.0", 2)
	if f.release.Verified() {
		t.Fatal("fixture is unexpectedly verified")
	}
	p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if err != nil {
		return
	}
	defer p.Close()
	_, err = f.engine.Apply(t.Context(), p, p.Digest(), nil)
	if err != nil {
		t.Fatalf("unsigned chart was rendered before refusal: %v", err)
	}
	t.Fatal("unsigned local chart rendered and applied over a deployed installation")
}

type historyTargetKube struct {
	*kubefake.PrintingKubeClient
	injected           bool
	lastBuildHasMarker bool
}

func (k *historyTargetKube) Build(r io.Reader, _ bool) (kube.ResourceList, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	k.lastBuildHasMarker = bytes.Contains(b, []byte("review-history-injection"))
	return kube.ResourceList{}, nil
}
func (k *historyTargetKube) Update(_ kube.ResourceList, target kube.ResourceList, _ bool) (*kube.Result, error) {
	k.injected = k.injected || k.lastBuildHasMarker
	return &kube.Result{}, nil
}

func TestReviewHistoryCannotSupplyAppliedBytes(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"manifest", "hook", "both"} {
		t.Run(shape, func(t *testing.T) { reviewHistoryTarget(t, shape) })
	}
}
func reviewHistoryTarget(t *testing.T, shape string) {
	t.Helper()
	store, request := reviewHistoryFixture(t)
	old, err := store.Get("oberth", 2)
	if err != nil {
		t.Fatal(err)
	}
	old.Chart = installer.ReleaseChartForTest(loadFixture(t, releaseFor(t, "v0.0.9", 1)))
	old.Labels = installer.RevisionLabelsForTest(installer.ChartRequest{Chart: old.Chart, Revision: old.Version - 1})
	poisonHistoryTarget(old, shape)
	if err = store.Update(old); err != nil {
		t.Fatal(err)
	}
	checked := loadFixture(t, releaseFixture(t))
	request.Chart = installer.ReleaseChartForTest(checked)
	kc := &historyTargetKube{PrintingKubeClient: &kubefake.PrintingKubeClient{Out: io.Discard}}
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.30.0", Major: "1", Minor: "30"}
	port := installer.ChartForTest(&action.Configuration{Releases: store, KubeClient: kc, Capabilities: caps, Log: func(string, ...any) {}})
	bindChartHistory(t, port, &request)
	err = port.Apply(t.Context(), request)
	if kc.injected {
		t.Fatal("unsigned history manifest reached native Kubernetes Update before fresh verified chart")
	}
	if err != nil {
		t.Fatal("fresh verified repair failed", err)
	}
	last, err := store.Last("oberth")
	if err != nil || last.Version != 4 || last.Info.Status != release.StatusDeployed || last.Chart.Metadata.AppVersion != checked.Version() {
		t.Fatal("fresh verified chart was not deployed", err)
	}
	if strings.Contains(last.Manifest, "review-history-injection") {
		t.Fatal("history entered fresh manifest")
	}

}

func TestReviewRecoveryFileShapes(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"missing", "directory", "outside symlink", "wide file"} {
		t.Run(shape, func(t *testing.T) { reviewRecoveryFileShape(t, shape) })
	}
}
func reviewRecoveryFileShape(t *testing.T, shape string) {
	t.Helper()
	f, backend := storeEngine(t)
	result := apply(t, f, prepare(t, f))
	path := result.Profile.RecoveryKey
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	mutateRecoveryFile(t, path, shape)
	backend.Sealed = true
	before := len(backend.Calls)
	_, err := f.engine.Unseal(t.Context())
	if err == nil || len(backend.Calls) != before || !backend.Sealed {
		t.Fatal("unsafe recovery input reached store")
	}
	if strings.Contains(err.Error(), "synthetic-review-value") {
		t.Fatal("key content reached diagnostic")
	}
}

func TestReviewReleaseInputShapes(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"missing", "empty", "directory", "symlink", "tampered", "record-chart-version", "replayed-version", "replaced-after-verify"} {
		t.Run(shape, func(t *testing.T) { reviewReleaseInputShape(t, shape) })
	}
}
func reviewReleaseInputShape(t *testing.T, shape string) {
	t.Helper()
	f := releaseFixture(t)
	switch shape {
	case "missing":
		delete(f.files, "oberth.tgz")
	case "empty":
		f.files["oberth.tgz"].Data = nil
	case "directory":
		f.files["oberth.tgz"].Mode = fs.ModeDir | 0700
	case "symlink":
		f.files["oberth.tgz"].Mode = fs.ModeSymlink | 0600
	case "tampered":
		f.files["oberth.tgz"].Data[len(f.files["oberth.tgz"].Data)/2] ^= 1
	case "record-chart-version":
		var record releaseverify.Record
		if err := json.Unmarshal(f.files["release.json"].Data, &record); err != nil {
			t.Fatal(err)
		}
		record.Version = "v0.2.0"
		f.files["release.json"].Data, _ = json.Marshal(record)
		signRelease(t, f.files)
		f.version = record.Version
	case "replayed-version":
		f.version = "v0.2.0"
	case "replaced-after-verify":
		reviewReplacedArtifact(t, f)
		return
	}
	if _, err := installer.LoadRelease(t.Context(), f.files, f.version); err == nil {
		t.Fatal("untrusted release shape accepted")
	}
}

func TestReviewRenderedAuthority(t *testing.T) {
	t.Parallel()
	for _, o := range rendered(t, nil) {
		kind, _ := o["kind"].(string)
		if kind == "Role" || kind == "ClusterRole" {
			reviewRoleAuthority(t, o)
		}
		if kind == "Role" || kind == "ClusterRole" || kind == "RoleBinding" || kind == "ClusterRoleBinding" || kind == "ServiceAccount" {
			t.Log(jsonOf(t, o))
		}
	}
}

func mutateRecoveryFile(t *testing.T, path, shape string) {
	t.Helper()
	switch shape {
	case "directory":
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	case "outside symlink":
		outside := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(outside, []byte("synthetic-review-value"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
	case "wide file":
		if err := os.WriteFile(path, []byte("synthetic-review-value"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func reviewReplacedArtifact(t *testing.T, f releaseData) {
	t.Helper()
	checked, err := releaseverify.Verify(t.Context(), f.files, f.version)
	if err != nil {
		t.Fatal(err)
	}
	f.files["oberth.tgz"].Data[len(f.files["oberth.tgz"].Data)/2] ^= 1
	data, err := checked.ReadFile("oberth.tgz", releaseverify.MaxArtifact)
	if !errors.Is(err, releaseverify.Digest) || data != nil {
		t.Fatal("post-verification replacement accepted")
	}
}

func reviewHistoryFixture(t *testing.T) (*storage.Storage, installer.ChartRequest) {
	t.Helper()
	_, initial, request := pendingFixture(t, true)
	store := storage.Init(installer.ReleaseSecretsForTest(clientfake.NewClientset().CoreV1().Secrets("oberth")))
	for number := 1; number <= 3; number++ {
		stored, err := initial.Get("oberth", number)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.Create(stored); err != nil {
			t.Fatal(err)
		}
	}
	return store, request
}

func reviewRoleAuthority(t *testing.T, o map[string]any) {
	t.Helper()
	for _, raw := range o["rules"].([]any) {
		rule := raw.(map[string]any)
		for _, resource := range rule["resources"].([]any) {
			if resource == "secrets" || resource == "*" {
				t.Fatalf("rendered role grants key access: %s", o["metadata"].(map[string]any)["name"])
			}
		}
	}
}

func poisonHistoryTarget(old *release.Release, shape string) {
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: review-history-injection\n  namespace: oberth\ndata:\n  source: unsigned-history\n"
	if shape != "hook" {
		old.Manifest = manifest
	}
	if shape != "manifest" {
		old.Hooks = []*release.Hook{{Name: "review-history-injection", Kind: "ConfigMap", Path: "templates/history.yaml", Manifest: manifest, Events: []release.HookEvent{release.HookPreRollback, release.HookPreUpgrade, release.HookPreDelete, release.HookPostDelete}}}
	}
}
func (k *historyTargetKube) Create(resources kube.ResourceList) (*kube.Result, error) {
	k.injected = k.injected || k.lastBuildHasMarker
	return &kube.Result{}, nil
}
