package installer_test

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	ktesting "k8s.io/client-go/testing"
)

func TestChartHTTPSServicePortDefaultsTo8443(t *testing.T) {
	t.Parallel()
	service := object(t, rendered(t, nil), "Service", "oberth")["spec"].(map[string]any)
	port := service["ports"].([]any)[0].(map[string]any)
	if port["port"] != float64(8443) {
		t.Fatal("chart default changed the existing HTTPS Service port", port)
	}
}

func TestHTTPSServiceServes443WithTheSelectedNodePort(t *testing.T) {
	t.Parallel()
	checkHTTPSServicePort(t, 443, 443)
}

func TestHTTPSServiceKeepsItsDefaultAndExplicitPorts(t *testing.T) {
	t.Parallel()
	for _, port := range []int{0, 8443, 9443} {
		t.Run(fmt.Sprint(port), func(t *testing.T) {
			want := port
			if want == 0 {
				want = 8443
			}
			checkHTTPSServicePort(t, port, want)
		})
	}
}

func checkHTTPSServicePort(t *testing.T, port, want int) {
	t.Helper()
	f := setup(t)
	f.choices.HTTPSServicePort = port
	f.choices.HTTPSNodePort = 31443
	f.choices.URL = "https://public.oberth.ci"
	p := prepare(t, f)
	values := maps.Clone(f.charts.Previews[0].Values)
	values["egressNetwork"] = networkValues(false)["egressNetwork"]
	objects := rendered(t, values)
	service := object(t, objects, "Service", "oberth")["spec"].(map[string]any)
	ports := service["ports"].([]any)
	https := ports[0].(map[string]any)
	assertHTTPSServiceMapping(t, service, https, want)
	if !strings.Contains(strings.Join(p.Review(), "\n"), fmt.Sprintf("HTTPS Service port: %d (--service-port)", want)) {
		t.Fatal("review omitted the in-cluster Service port", p.Review())
	}
	var network networkingv1.NetworkPolicy
	decodeObject(t, object(t, objects, "NetworkPolicy", "oberth-egress"), &network)
	checkRenderedServerPorts(t, network, objects)
	checkSourceServiceEndpoint(t, objects, f.charts.Previews[0].Values)
	checkServicePortDryRun(t, f, want)
	result, err := f.engine.Apply(t.Context(), p, p.Digest(), nil)
	if err != nil || result.Profile.HTTPSNodePort != 31443 || result.Profile.ServerURL != f.choices.URL || result.Profile.HTTPSServicePort != want {
		t.Fatal("Service port changed the public endpoint", result.Profile, err)
	}
	if installer.Defaults().HTTPSNodePort != 30443 || installer.Defaults().HTTPSServicePort != 8443 {
		t.Fatal("public default node port changed")
	}
}

func checkServicePortDryRun(t *testing.T, f fixture, port int) {
	t.Helper()
	found := false
	for _, action := range f.client.Actions() {
		if action.GetVerb() != "create" || action.GetResource().Resource != "services" {
			continue
		}
		service := action.(ktesting.CreateAction).GetObject().(*corev1.Service)
		if !strings.HasPrefix(service.GenerateName, "oberth-port-check-") {
			continue
		}
		found = true
		if service.Spec.Ports[0].Port != int32(port) || service.Spec.Ports[1].Port != 2222 {
			t.Fatal("dry-run Service differs from the reviewed chart ports", service.Spec.Ports)
		}
	}
	if !found {
		t.Fatal("Service port was not checked through the dry-run API")
	}
}

func assertHTTPSServiceMapping(t *testing.T, service, https map[string]any, port int) {
	t.Helper()
	if service["type"] != "NodePort" || len(service["ports"].([]any)) != 2 || https["name"] != "https" || https["port"] != float64(port) || https["targetPort"] != "https" || https["nodePort"] != float64(31443) {
		t.Fatal("proxy cannot use its selected HTTPS port or public mapping changed", service)
	}
}

func checkSourceServiceEndpoint(t *testing.T, objects []map[string]any, values map[string]any) {
	t.Helper()
	source := object(t, objects, "Service", "oberth-source")
	port := source["spec"].(map[string]any)["ports"].([]any)[0].(map[string]any)["port"]
	want := fmt.Sprintf("https://oberth-source.oberth.svc:%v", port)
	runtime := values["server"].(map[string]any)["runtime"].(map[string]any)
	endpoint := runtime["source"].(map[string]any)["endpoint"]
	controller := object(t, objects, "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
	if endpoint != want || !strings.Contains(controller["executor"].(string), want) {
		t.Fatal("source plugin and inherited helper endpoint differ from their Service", endpoint, controller["executor"], want)
	}
}

func TestHTTPSServicePortUpgradeReviewAndSavedChoice(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	f.charts.History.Deployed.Version = "v0.1.0"
	f.charts.History.Latest = f.charts.History.Deployed
	setInstalledMetadata(t, f, "local", 6)
	m := installer.Maintenance{Engine: f.engine, Options: stateport.Options{Schema: 6, Protocol: stateport.DefaultProtocol()}, ActiveRuns: func(context.Context, string) (int, error) { return 0, nil }, ReleaseFiles: func(ctx context.Context, version string) (fs.FS, error) {
		return releaseFor(t, version, 6).files, ctx.Err()
	}}
	target := loadFixture(t, releaseFor(t, "v0.2.0", 6))
	upgrade, err := m.PlanUpgrade(t.Context(), target, installer.Choices{Context: "test", HTTPSServicePort: 443}, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer upgrade.Plan.Close()
	if !strings.Contains(strings.Join(upgrade.Plan.Review(), "\n"), "HTTPS Service port: 8443 -> 443 (--service-port)") {
		t.Fatal("upgrade hid its Service port change", upgrade.Plan.Review())
	}
	if _, err = m.Upgrade(t.Context(), upgrade, upgrade.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	assertServicePortInherited(t, f, target)
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f.charts.Applies[len(f.charts.Applies)-1].Values["servicePort"]) != "8443" {
		t.Fatal("rollback did not restore its recorded Service port")
	}
}

func assertServicePortInherited(t *testing.T, f fixture, release installer.Release) {
	t.Helper()
	p, err := f.engine.Prepare(t.Context(), release, installer.Choices{Context: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	review := strings.Join(p.Review(), "\n")
	if !strings.Contains(review, "HTTPS Service port: 443 (--service-port)") || strings.Contains(review, "HTTPS Service port: 8443 ->") || fmt.Sprint(f.charts.Previews[len(f.charts.Previews)-1].Values["servicePort"]) != "443" {
		t.Fatal("saved Service port changed or unchanged review shows an arrow", review)
	}
}
