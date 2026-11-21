package installer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func delta2Native(t *testing.T, f fixture, pending, poison bool) (*storage.Storage, *deltaDeletionKube) {
	t.Helper()
	store := storage.Init(installer.ReleaseSecretsForTest(f.client.CoreV1().Secrets("oberth")))
	rev := f.charts.History.Latest
	ch := installer.ReleaseChartForTest(f.release)
	record := &release.Release{Name: "oberth", Namespace: "oberth", Version: rev.Number, Chart: ch, Config: rev.Values, Info: &release.Info{Status: release.StatusDeployed}, Labels: installer.RevisionLabelsForTest(installer.ChartRequest{Chart: ch, Revision: rev.Number - 1})}
	if poison {
		poisonHistoryTarget(record, "both")
	}
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	if pending {
		record.Version++
		record.Info = &release.Info{Status: release.StatusPendingUpgrade}
		record.Labels = installer.RevisionLabelsForTest(installer.ChartRequest{Chart: ch, Revision: record.Version - 1})
		if err := store.Create(record); err != nil {
			t.Fatal(err)
		}
	}
	kc := deltaDeleteClient(t)
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.30.0", Major: "1", Minor: "30"}
	f.engine.Charts = installer.ChartForTest(&action.Configuration{Releases: store, KubeClient: kc, Capabilities: caps, Log: func(string, ...any) {}})
	return store, kc
}

func TestDelta2EngineDeletionRefusesBeforeCredentialWrites(t *testing.T) {
	t.Parallel()
	for _, pending := range []bool{false, true} {
		t.Run(boolText(pending), func(t *testing.T) { delta2EngineDeletionCase(t, pending) })
	}
}
func delta2EngineDeletionCase(t *testing.T, pending bool) {
	t.Helper()
	f := setup(t)
	f.release = loadFixture(t, releaseFixture(t))
	apply(t, f, prepare(t, f))
	store, kc := delta2Native(t, f, pending, pending)
	beforeAdmin, err := f.client.CoreV1().Secrets("oberth").Get(t.Context(), "oberth-admin", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan := delta2PrepareForDeletion(t, f)
	if plan == nil {
		return
	}
	defer plan.Close()
	if !pending {
		changeRevision(t, store, 1, func(record *release.Release) { poisonHistoryTarget(record, "both") })
	}
	actions := len(f.client.Actions())
	beforeHistory := deltaHistoryBytes(t, store)
	_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
	var refusal *installer.Problem
	code := "history-delete"
	if !pending {
		code = "target"
	}
	if !errors.As(err, &refusal) || refusal.Code != code {
		t.Fatalf("expected deletion refusal, got %v", err)
	}
	if kc.updates != 0 || len(kc.deleted) != 0 || !bytes.Equal(beforeHistory, deltaHistoryBytes(t, store)) {
		t.Fatal("deletion/history escaped native guard")
	}
	delta2AssertCredentialsUnchanged(t, f, actions, beforeAdmin.Data["admin.pub"])
}
func delta2PrepareForDeletion(t *testing.T, f fixture) *installer.Plan {
	t.Helper()
	beforePrepare := len(f.client.Actions())
	plan, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if err != nil {
		var p *installer.Problem
		if !errors.As(err, &p) || !strings.Contains(err.Error(), "Stored history would delete") {
			t.Fatal(err)
		}
		deltaAssertOnlyClusterReads(t, f, beforePrepare)
		if _, e := os.Stat(f.dir + "/installation.json"); e != nil {
			t.Fatal("planning refusal lost original profile", e)
		}
		return nil
	}
	return plan
}

func delta2AssertCredentialsUnchanged(t *testing.T, f fixture, actions int, beforePublicKey []byte) {
	t.Helper()
	afterAdmin, getErr := f.client.CoreV1().Secrets("oberth").Get(t.Context(), "oberth-admin", metav1.GetOptions{})
	if getErr != nil {
		t.Fatal(getErr)
	}
	_, profileErr := os.Stat(f.dir + "/installation.json")
	var writes []string
	for _, a := range f.client.Actions()[actions:] {
		if !readOnly(a) {
			writes = append(writes, a.GetVerb()+" "+a.GetResource().Resource)
		}
	}
	if len(writes) > 0 || profileErr != nil || !bytes.Equal(beforePublicKey, afterAdmin.Data["admin.pub"]) {
		t.Fatalf("deletion refused AFTER writes: cluster=%v profileLost=%t adminReplaced=%t", writes, profileErr != nil, !bytes.Equal(beforePublicKey, afterAdmin.Data["admin.pub"]))
	}
}

func TestDelta2PendingRollbackDeletionRefusesBeforeRestore(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 2))
	plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	store, kc := delta2Native(t, f, false, true)
	last, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	last.Info.Status = release.StatusPendingUpgrade
	if err = store.Update(last); err != nil {
		t.Fatal(err)
	}
	before := deltaHistoryBytes(t, store)
	err = m.Rollback(t.Context())
	if err == nil {
		t.Fatal("poisoned rollback accepted")
	}
	if kc.updates != 0 || len(kc.deleted) != 0 || !bytes.Equal(before, deltaHistoryBytes(t, store)) {
		t.Fatal("deletion/history escaped native guard")
	}
}

func TestDelta2ReleaseMetadataLivesOnSecret(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.release = loadFixture(t, releaseFixture(t))
	apply(t, f, prepare(t, f))
	store, _ := delta2Native(t, f, false, false)
	items, err := f.client.CoreV1().Secrets("oberth").List(t.Context(), metav1.ListOptions{LabelSelector: "owner=helm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items.Items) != 1 {
		t.Fatal("missing native release secret")
	}
	s := items.Items[0]
	t.Logf("provenance object=%s/%s type=%s labelKind=%s binding=%s", s.Namespace, s.Name, s.Type, s.Labels["oberth.ci/release-kind"], s.Labels["oberth.ci/kind-revision"])
	// A writer of this Secret can recompute the non-secret binding. This is an
	// authority observation, not a signature-forgery or product-boundary claim.
	s.Labels["oberth.ci/release-kind"] = "local"
	s.Labels["oberth.ci/kind-revision"] = installer.RevisionBindingForTest(1, "local", f.release.Version())
	if _, err = f.client.CoreV1().Secrets("oberth").Update(t.Context(), &s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	h, err := f.engine.Charts.Read(context.Background(), "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if h.Verified || h.Latest.Kind != "local" {
		t.Fatal("metadata fixture did not establish authority", h)
	}
	local := localVersion(t, f.release.Version(), 1)
	p, err := f.engine.Prepare(t.Context(), local, f.choices)
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	if _, err = store.Last("oberth"); err != nil {
		t.Fatal(err)
	}
}

func TestDelta2MalformedReleasePayloadIsNotFreshInstallation(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.release = loadFixture(t, releaseFixture(t))
	apply(t, f, prepare(t, f))
	_, _ = delta2Native(t, f, false, false)
	items, err := f.client.CoreV1().Secrets("oberth").List(t.Context(), metav1.ListOptions{LabelSelector: "owner=helm"})
	if err != nil {
		t.Fatal(err)
	}
	s := items.Items[0]
	s.Data["release"] = []byte("invalid")
	if _, err = f.client.CoreV1().Secrets("oberth").Update(t.Context(), &s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	h, err := f.engine.Charts.Read(t.Context(), "oberth")
	if err == nil {
		raw, _ := json.Marshal(h)
		t.Fatalf("malformed existing verified Secret treated as readable history: %s", raw)
	}
}
