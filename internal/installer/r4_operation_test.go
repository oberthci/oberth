package installer_test

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/release"
)

func TestMaintenanceDeletionRefusesBeforeRecordBackupAndRestore(t *testing.T) {
	t.Parallel()
	maintenanceDeletionCase(t)
}

func maintenanceDeletionCase(t *testing.T) {
	t.Helper()
	f, m := maintenanceFixture(t)
	f.release = loadFixture(t, releaseFor(t, "v0.0.9", 1))
	store, kc := delta2Native(t, f, false, false)
	next := loadFixture(t, releaseFor(t, "v0.1.0", 1))
	plan, err := m.PlanUpgrade(t.Context(), next, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	old, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	poisonHistoryTarget(old, "both")
	if err = store.Update(old); err != nil {
		t.Fatal(err)
	}
	before, history, actions := directoryBytes(t, f.dir), deltaHistoryBytes(t, store), len(f.client.Actions())
	m.Source = snapshotCheck{m.Source, func() { t.Fatal("backup started before deletion refusal") }}
	_, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "target" {
		t.Fatal("expected deletion refusal", err)
	}
	if !reflect.DeepEqual(before, directoryBytes(t, f.dir)) || !bytes.Equal(history, deltaHistoryBytes(t, store)) || kc.updates != 0 {
		t.Fatal("maintenance wrote before deletion refusal")
	}
	deltaAssertOnlyClusterReads(t, f, actions)
}

func directoryBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, entry := range entries {
		raw, err := os.ReadFile(dir + "/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = string(raw)
	}
	return files
}

func TestExistingLocalPreviewChecksDeletion(t *testing.T) {
	t.Parallel()
	for _, pending := range []bool{false, true} {
		t.Run(boolText(pending), func(t *testing.T) { localDeletionPreviewCase(t, pending) })
	}
}
func localDeletionPreviewCase(t *testing.T, pending bool) {
	t.Helper()
	_, store, request := pendingFixture(t, true)
	request.Local = true
	if !pending {
		request.Recovery = nil
	}
	for number := 1; number <= 3; number++ {
		record, err := store.Get("oberth", number)
		if err != nil {
			t.Fatal(err)
		}
		record.Labels = installer.RevisionLabelsForTest(installer.ChartRequest{Chart: record.Chart, Revision: number - 1, Local: true})
		if number >= 2 {
			poisonHistoryTarget(record, "both")
		}
		if number == 3 && !pending {
			record.Info.Status = release.StatusDeployed
		}
		if err = store.Update(record); err != nil {
			t.Fatal(err)
		}
	}
	port := installer.ChartForTest(&action.Configuration{Releases: store, KubeClient: deltaDeleteClient(t), Capabilities: chartutil.DefaultCapabilities, Log: func(string, ...any) {}})
	bindChartHistory(t, port, &request)
	before := deltaHistoryBytes(t, store)
	var refusal *installer.Problem
	if err := port.Preview(t.Context(), request); !errors.As(err, &refusal) || refusal.Code != "history-delete" {
		t.Fatal("existing local recovery preview skipped deletion preflight")
	}
	if !bytes.Equal(before, deltaHistoryBytes(t, store)) {
		t.Fatal("preview changed pending history")
	}
}
