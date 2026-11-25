package installer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/internal/stateport"
	"helm.sh/helm/v3/pkg/chart/loader"
)

// Only the release envelope uses the test signer; chart bytes stay archived.
func historicalStatusRelease(t *testing.T) releaseData {
	t.Helper()
	f := archivedRelease(t, "v0.30.14", "c0362d5e9c38c51d2a0d5c1b2b003f195efc4ca67d14b802aa4f32d398b1c86f")
	ch, err := loader.LoadArchive(bytes.NewReader(f.files["oberth.tgz"].Data))
	if err != nil || bytes.Contains(ch.Schema, []byte(`"statusChosen"`)) {
		t.Fatal("historical chart fixture differs", err)
	}
	return f
}

func archivedRelease(t *testing.T, version, digest string) releaseData {
	t.Helper()
	archive, err := os.ReadFile("testdata/oberth-" + version + ".tgz")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(archive)) != digest {
		t.Fatal("released chart fixture checksum differs")
	}
	ch, err := loader.LoadArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if ch.Metadata.AppVersion != version {
		t.Fatal("historical chart fixture differs")
	}
	record, err := json.Marshal(releaseverify.Record{Version: ch.Metadata.AppVersion, Commit: strings.Repeat("a", 40), Images: map[string]string{"oberth": ch.Values["imageDigest"].(string), "oberth-source-plugin": ch.Values["sourcePluginImageDigest"].(string)}, Base: "sha256:" + strings.Repeat("d", 64)})
	if err != nil {
		t.Fatal(err)
	}
	f := releaseData{version: ch.Metadata.AppVersion, files: fstest.MapFS{"oberth.tgz": {Data: archive}, "release.json": {Data: record}}}
	signRelease(t, f.files)
	return f
}

func TestRollbackToReleasedChartUsesItsDeclaredValues(t *testing.T) {
	rollbackToReleasedChart(t, historicalStatusRelease(t), []string{"statusChosen", "statusOrigins", "statusPodSecurity", "egressNetwork"})
}

func TestRollbackToPreviousReleaseHasNoMissingPriorityClass(t *testing.T) {
	old := archivedRelease(t, "v0.30.15", "508a087f17d41cc00b710590a840404424859a0346e96b019ea526a38e69effd")
	rollbackToReleasedChart(t, old, []string{"egressNetwork"})
}

func rollbackToReleasedChart(t *testing.T, old releaseData, undeclared []string) {
	t.Helper()
	f := setup(t)
	apply(t, f, prepare(t, f))
	setHistoricalRevision(t, f, old)
	target := loadFixture(t, releaseFor(t, "v0.31.0", 6))
	m := installer.Maintenance{Engine: f.engine, ActiveRuns: func(context.Context, string) (int, error) { return 0, nil }, Options: stateport.Options{Schema: 6, Protocol: stateport.DefaultProtocol()}, ReleaseFiles: func(ctx context.Context, version string) (fs.FS, error) {
		if version != old.version {
			t.Fatal("wrong rollback release", version)
		}
		return old.files, ctx.Err()
	}}
	p, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	if _, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal("released chart rollback refused", err)
	}
	last := f.charts.Applies[len(f.charts.Applies)-1]
	if last.Chart.Metadata.AppVersion != old.version {
		t.Fatal("rollback did not apply the released chart")
	}
	files, err := installer.Render(last.Chart, last.Values, last.Namespace)
	if err != nil {
		t.Fatal("released chart does not validate", err)
	}
	assertNoPriorityReferences(t, files)
	for _, name := range undeclared {
		if _, exists := last.Values[name]; exists {
			t.Fatal("undeclared value reached the old chart", name)
		}
	}
}

func assertNoPriorityReferences(t *testing.T, files map[string]string) {
	t.Helper()
	for _, resource := range chartObjects(t, files) {
		if resource["kind"] == "CustomResourceDefinition" {
			continue
		}
		if resource["kind"] == "PriorityClass" || strings.Contains(jsonOf(t, resource["spec"]), `"priorityClassName":`) {
			t.Fatal("rollback references a priority class absent from the released chart", resource["kind"], resource["metadata"])
		}
	}
}

func setHistoricalRevision(t *testing.T, f fixture, old releaseData) {
	t.Helper()
	ch, err := loader.LoadArchive(bytes.NewReader(old.files["oberth.tgz"].Data))
	if err != nil {
		t.Fatal(err)
	}
	previous := f.charts.History.Deployed
	previous.Version, previous.Kind = old.version, "verified"
	for _, name := range []string{"policies", "argoNative", "storeNative"} {
		previous.Values[name] = ch.Values[name]
	}
	for _, name := range []string{"egressNetwork", "statusChosen", "statusOrigins", "statusPodSecurity"} {
		delete(previous.Values, name)
	}
	if _, err = installer.Render(ch, previous.Values, f.choices.Namespace); err != nil {
		t.Fatal("historical revision does not validate", err)
	}
	f.charts.History.Deployed, f.charts.History.Latest = previous, previous
	setInstalledMetadata(t, f, "verified", 6)
}
