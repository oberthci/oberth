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

// The chart bytes are the archived v0.30.14 release, not this checkout's chart
// with a different version. Only its release envelope uses the test signer.
func historicalStatusRelease(t *testing.T) releaseData {
	t.Helper()
	archive, err := os.ReadFile("testdata/oberth-v0.30.14.tgz")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(archive)) != "c0362d5e9c38c51d2a0d5c1b2b003f195efc4ca67d14b802aa4f32d398b1c86f" {
		t.Fatal("released chart fixture checksum differs")
	}
	ch, err := loader.LoadArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if ch.Metadata.AppVersion != "v0.30.14" || bytes.Contains(ch.Schema, []byte(`"statusChosen"`)) {
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
	f := setup(t)
	apply(t, f, prepare(t, f))
	old := historicalStatusRelease(t)
	oldChart, err := loader.LoadArchive(bytes.NewReader(old.files["oberth.tgz"].Data))
	if err != nil {
		t.Fatal(err)
	}
	// Native history contains the released policy shape, without later network
	// discovery or status metadata.
	previous := f.charts.History.Deployed
	previous.Version, previous.Kind = old.version, "verified"
	previous.Values["policies"] = oldChart.Values["policies"]
	delete(previous.Values, "egressNetwork")
	delete(previous.Values, "statusChosen")
	delete(previous.Values, "statusOrigins")
	delete(previous.Values, "statusPodSecurity")
	if _, err = installer.Render(oldChart, previous.Values, f.choices.Namespace); err != nil {
		t.Fatal("historical revision does not validate", err)
	}
	f.charts.History.Deployed, f.charts.History.Latest = previous, previous
	setInstalledMetadata(t, f, "verified", 6)
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
	if _, err = installer.Render(last.Chart, last.Values, last.Namespace); err != nil {
		t.Fatal("released chart does not validate", err)
	}
	for _, name := range []string{"statusChosen", "statusOrigins", "statusPodSecurity", "egressNetwork"} {
		if _, exists := last.Values[name]; exists {
			t.Fatal("undeclared value reached the old chart", name)
		}
	}
}
