package installer_test

import (
	"testing"

	"helm.sh/helm/v3/pkg/release"
)

func TestRemovedInstallerChoicesSurvivePrunedForeignHistory(t *testing.T) {
	t.Parallel()
	f, store, _ := foreignInstallFixture(t, false)
	appendForeignRevisions(t, store)
	number := installerAttempt(t, store, release.StatusFailed)
	changeRevision(t, store, number, func(r *release.Release) {
		r.Config["database"] = map[string]any{"file": "/var/lib/oberth/custom.db"}
		r.Config["runs"] = map[string]any{"parallel": 5}
		r.Info.Status = release.StatusUninstalled
	})
	changeRevision(t, store, 3, func(r *release.Release) { r.Info.Status = release.StatusSuperseded })
	if _, err := store.Delete("oberth", 1); err != nil {
		t.Fatal(err)
	}
	before := deltaHistoryBytes(t, store)
	p := prepare(t, f)
	defer p.Close()
	if string(before) != string(deltaHistoryBytes(t, store)) {
		t.Fatal("review changed removed history")
	}
	apply(t, f, p)
	assertStoredParallel(t, store, 5)
	last, err := store.Last("oberth")
	if err != nil || last.Config["database"].(map[string]any)["file"] != "/var/lib/oberth/custom.db" {
		t.Fatal("native recovery lost the removed revision's database choice", err)
	}
}
