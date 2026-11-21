package installer_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/installer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSecondInstallationAnswersSurviveRerunAndUpgrade(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices = installer.Choices{Context: "test", Namespace: "second", PipelineNamespace: "jobs", HTTPSNodePort: 31443, SSHNodePort: 31022, URL: "https://ci.example:443", SSHPort: 2222}
	definitionFixture(t, f, "control")
	p := prepare(t, f)
	review := strings.Join(p.Review(), "\n")
	for _, want := range []string{"Namespaces: second / jobs", "HTTPS: https://ci.example:443 -> node port 31443", "SSH: ci.example:2222 -> node port 31022"} {
		if !strings.Contains(review, want) {
			t.Fatal("public mapping missing", want, review)
		}
	}
	result := apply(t, f, p)
	if result.Profile.Namespace != "second" {
		t.Fatal(result.Profile)
	}
	store, err := f.client.CoreV1().Secrets("second").Get(t.Context(), "oberth-store-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := clientcert.ParseCertificate(store.Data["tls.crt"])
	if err != nil || cert.VerifyHostname("oberth-store.second.svc") != nil {
		t.Fatal("store certificate uses wrong namespace", err)
	}
	f.choices = installer.Choices{Context: "test"}
	rerun := prepare(t, f)
	apply(t, f, rerun)
	assertSecondAnswers(t, f.charts.Applies[len(f.charts.Applies)-1])
	f.charts.History.Deployed.Version = "v0.0.9"
	f.charts.History.Latest = f.charts.History.Deployed
	setInstalledMetadata(t, f, "local", 1)
	f.release = loadFixture(t, releaseFor(t, "v0.0.10", 4))
	source, options := backupFixture(t)
	options.Schema = 4
	m := installer.Maintenance{Engine: f.engine, Source: source, Options: options, ReleaseFiles: func(ctx context.Context, version string) (fs.FS, error) {
		return releaseFor(t, version, 4).files, ctx.Err()
	}, ActiveRuns: func(_ context.Context, ns string) (int, error) {
		if ns != "jobs" {
			t.Fatal("upgrade counted another namespace", ns)
		}
		return 0, nil
	}}
	upgrade, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer upgrade.Plan.Close()
	if _, err = m.Upgrade(t.Context(), upgrade, upgrade.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	assertSecondAnswers(t, f.charts.Applies[len(f.charts.Applies)-1])
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertSecondAnswers(t, f.charts.Applies[len(f.charts.Applies)-1])
}

func assertSecondAnswers(t *testing.T, request installer.ChartRequest) {
	t.Helper()
	v := request.Values
	if request.Namespace != "second" || v["pipeline"].(map[string]any)["namespace"] != "jobs" {
		t.Fatal("namespace answer lost", request.Namespace, v["pipeline"])
	}
	if jsonOf(t, v["nodePorts"]) != `{"https":31443,"ssh":31022}` || jsonOf(t, v["server"].(map[string]any)["public"]) != `{"ssh_port":2222,"url":"https://ci.example:443"}` {
		t.Fatal("port or public endpoint answer lost", v)
	}
	if strings.Contains(jsonOf(t, v), ".oberth.svc") {
		t.Fatal("default namespace leaked into stored values")
	}
	files, err := installer.Render(request.Chart, request.Values, request.Namespace)
	if err != nil || strings.Contains(jsonOf(t, files), "kind: CustomResourceDefinition") {
		t.Fatal("upgrade rendered shared definitions", err)
	}
}

func TestChartCustomNodePorts(t *testing.T) {
	t.Parallel()
	objects := renderedIn(t, map[string]any{"nodePorts": map[string]any{"https": 31443, "ssh": 31022}}, "second")
	service := object(t, objects, "Service", "oberth")
	ports := service["spec"].(map[string]any)["ports"].([]any)
	for i, want := range []float64{31443, 31022} {
		if ports[i].(map[string]any)["nodePort"] != want {
			t.Fatal("custom node ports ignored", service)
		}
	}
}

func TestInstallationProfileCannotCrossNamespaces(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	f.choices.Namespace = "second"
	_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if err == nil || !strings.Contains(err.Error(), "another namespace") {
		t.Fatal("profile reused for another installation", err)
	}
}

func TestInvalidSecondInstallationChoices(t *testing.T) {
	t.Parallel()
	for _, choices := range []installer.Choices{
		{Namespace: "Invalid"}, {Namespace: "second", PipelineNamespace: "second"},
		{HTTPSNodePort: -1}, {SSHNodePort: 65536}, {HTTPSNodePort: 30022},
	} {
		f := setup(t)
		choices.Context = "test"
		if _, err := f.engine.Prepare(t.Context(), f.release, choices); err == nil {
			t.Fatal("invalid choices accepted", choices)
		}
		assertNoInstallWrites(t, f)
	}
}

func TestExternalStoreRequiresItsOwnAddress(t *testing.T) {
	t.Parallel()
	f := setup(t)
	bundled := false
	f.choices.Store = &bundled
	f.choices.Namespace = "second"
	if _, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil || !strings.Contains(err.Error(), "external secret store URL") {
		t.Fatal("chart default supplied an unchosen external store", err)
	}
	assertNoInstallWrites(t, f)
}
