package installer_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/kubeconfig"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"os"
	"slices"
	"testing"
)

func setInstalledMetadata(t *testing.T, f fixture, kind string, schema int) {
	t.Helper()
	path := f.dir + "/installation.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	data["release_kind"], data["server_schema"] = kind, schema
	data["server_version"] = f.charts.History.Deployed.Version
	raw, err = json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func localVersion(t *testing.T, version string, schema int) installer.Release {
	t.Helper()
	ch, err := loader.Load(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	files := make([]*loader.BufferedFile, 0, len(ch.Raw))
	for _, file := range ch.Raw {
		body := bytes.Clone(file.Data)
		if file.Name == "Chart.yaml" {
			body = bytes.Replace(body, []byte("appVersion: unset\n"), []byte("appVersion: "+version+"\n"), 1)
		}
		if file.Name == "values.yaml" {
			body = bytes.Replace(body, fmt.Appendf(nil, "targetSchema: %v\n", ch.Values["targetSchema"]), fmt.Appendf(nil, "targetSchema: %d\n", schema), 1)
		}
		files = append(files, &loader.BufferedFile{Name: file.Name, Data: body})
	}
	ch, err = loader.LoadFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	path, err := chartutil.Save(ch, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := installer.LocalRelease(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestLocalReleaseTransitions(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.release = localVersion(t, "v0.2.0", 2)
	apply(t, f, prepare(t, f))
	if f.charts.History.Latest.Kind != "local" {
		t.Fatal("local version metadata missing")
	}
	f.release = localVersion(t, "v0.2.0", 2)
	p := prepare(t, f)
	if !slices.Contains(p.Review(), "unverified local chart") {
		t.Fatal("local warning is not a separate review line")
	}
	if r := apply(t, f, p); f.charts.History.Latest.Kind != "local" || r.Profile.ServerVersion != f.release.Version() {
		t.Fatal("local-to-local update lost provenance")
	}
	verifyLocalCannotReplaceReleased(t, f)
}
func TestInstallRerunRefusesSchemaDowngrade(t *testing.T) {
	t.Parallel()
	for _, local := range []bool{false, true} {
		t.Run(boolText(local), func(t *testing.T) { installRerunSchemaCase(t, local) })
	}
}
func installRerunSchemaCase(t *testing.T, local bool) {
	t.Helper()
	f := setup(t)
	if !local {
		f.release = loadFixture(t, releaseFor(t, "v0.2.0", 2))
	}
	apply(t, f, prepare(t, f))
	if local {
		f.release = localVersion(t, "dev-older", 1)
	} else {
		f.release = loadFixture(t, releaseFor(t, "v0.1.0", 1))
	}
	before, err := os.ReadFile(f.dir + "/installation.json")
	if err != nil {
		t.Fatal(err)
	}
	previews := len(f.charts.Previews)
	p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if err == nil {
		p.Close()
		t.Fatal("install rerun accepted a lower schema")
	}
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "upgrade-required" {
		t.Fatal("wrong schema refusal", err)
	}
	after, err := os.ReadFile(f.dir + "/installation.json")
	if err != nil || !bytes.Equal(before, after) || len(f.charts.Previews) != previews {
		t.Fatal("schema refusal wrote or previewed", err)
	}
}

func TestLocalChartRenderingHasNoClusterDependency(t *testing.T) {
	t.Parallel()
	r, err := installer.LocalRelease(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	files, err := installer.Render(installer.ReleaseChartForTest(r), nil, "offline")
	if err != nil || len(files) == 0 {
		t.Fatal("offline chart rendering requires a cluster", err)
	}
	port := installer.NewChartLibrary(kubeconfig.Target{}, 0)
	if err = port.Preview(t.Context(), installer.ChartRequest{Local: true, Chart: installer.ReleaseChartForTest(r), Namespace: "offline"}); err != nil {
		t.Fatal("local preview initialized a cluster client", err)
	}
}
func TestInstalledReleaseWithoutSchemaRequiresMaintenance(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	setInstalledMetadata(t, f, "local", 0)
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 2))
	if p, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil {
		p.Close()
		t.Fatal("unknown database schema allowed a version change")
	}
}

func verifyLocalCannotReplaceReleased(t *testing.T, f fixture) {
	t.Helper()
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 2))
	apply(t, f, prepare(t, f))
	if f.charts.History.Latest.Kind != "verified" {
		t.Fatal("verified install lost provenance")
	}
	f.release = localVersion(t, "v0.2.0", 2)
	before, err := os.ReadFile(f.dir + "/installation.json")
	if err != nil {
		t.Fatal(err)
	}
	previews := len(f.charts.Previews)
	if p, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil {
		p.Close()
		t.Fatal("local chart replaced a verified release at the same version")
	}
	after, err := os.ReadFile(f.dir + "/installation.json")
	if err != nil || !bytes.Equal(before, after) || len(f.charts.Previews) != previews {
		t.Fatal("local refusal changed state or previewed", err)
	}
}

func TestLocalChartRequiresKnownMatchingProvenance(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"missing kind", "different version"} {
		t.Run(shape, func(t *testing.T) {
			f := setup(t)
			apply(t, f, prepare(t, f))
			store, _ := delta2Native(t, f, false, false)
			last, err := store.Last("oberth")
			if err != nil {
				t.Fatal(err)
			}
			last.Labels = installer.RevisionLabelsForTest(installer.ChartRequest{Chart: last.Chart, Local: true})
			expected := "release-unowned"
			if shape == "missing kind" {
				delete(last.Labels, "oberth.ci/release-kind")
			} else {
				last.Chart.Metadata.AppVersion = "different"
				last.Labels = installer.RevisionLabelsForTest(installer.ChartRequest{Chart: last.Chart, Local: true})
				expected = "upgrade-required"
			}
			if err = store.Update(last); err != nil {
				t.Fatal(err)
			}
			assertInstallRefusal(t, f, expected)
		})
	}
}
func TestApprovalBindsLocalChartWarning(t *testing.T) {
	t.Parallel()
	f := setup(t)
	fixture := releaseFor(t, "v0.2.0", 2)
	path := f.dir + "/local.tgz"
	if err := os.WriteFile(path, fixture.files["oberth.tgz"].Data, 0600); err != nil {
		t.Fatal(err)
	}
	local, err := installer.LocalRelease(path)
	if err != nil {
		t.Fatal(err)
	}
	f.release = local
	apply(t, f, prepare(t, f))
	localPlan := prepare(t, f)
	f.release = loadFixture(t, fixture)
	verifiedPlan := prepare(t, f)
	if localPlan.Digest() == verifiedPlan.Digest() {
		t.Fatal("approval digest omitted the local warning")
	}
}
