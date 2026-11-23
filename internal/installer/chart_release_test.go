package installer_test

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestChartHasExactProducerPlaceholders(t *testing.T) {
	t.Parallel()
	for name, lines := range map[string][]string{"Chart.yaml": {"version: 0.0.0\n", "appVersion: unset\n"}, "values.yaml": {"imageDigest: unset\n", "sourcePluginImageDigest: unset\n"}} {
		body, err := os.ReadFile(chartPath() + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range lines {
			if bytes.Count(body, []byte(line)) != 1 {
				t.Fatal("canonical producer placeholder missing or repeated", name, line)
			}
		}
	}
}
func TestSignedChartFactsReplaceHistoricalDefaults(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	old := f.charts.History.Deployed.Values
	old["targetSchema"] = 99
	old["imageDigest"] = "unset"
	old["server"].(map[string]any)["protocol"] = map[string]any{"schema_identity": "old", "audit_domain": "old"}
	old["server"].(map[string]any)["runtime"].(map[string]any)["executor_image"] = "old/executor:old"
	old["argoNative"] = map[string]any{"controller": map[string]any{"image": map[string]any{"tag": "old"}}}
	old["storeNative"] = map[string]any{"server": map[string]any{"image": map[string]any{"tag": "old"}}}
	f.charts.History.Latest = f.charts.History.Deployed
	f.release = loadFixture(t, releaseFor(t, "v0.1.0", 2))
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	values := f.charts.Previews[len(f.charts.Previews)-1].Values
	objects := rendered(t, values)
	server := jsonOf(t, object(t, objects, "Deployment", "oberth"))
	config := serverJSON(t, objects)
	source := "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth-source-plugin@sha256:" + strings.Repeat("c", 64)
	protocol := stateport.DefaultProtocol()
	if !strings.Contains(server, "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:"+strings.Repeat("b", 64)) || !reflect.DeepEqual(config["protocol"], map[string]any{"schema_identity": protocol.SchemaIdentity, "audit_domain": protocol.AuditDomain}) {
		t.Fatal("product or protocol came from history")
	}
	controller := object(t, objects, "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
	trust := object(t, objects, "ConfigMap", "oberth-source")["data"].(map[string]any)["source.json"].(string)
	if config["runtime"].(map[string]any)["helper_image"] != "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:"+strings.Repeat("b", 64) || !strings.Contains(controller["executor"].(string), "quay.io/argoproj/argoexec:v4.0.15") || !strings.Contains(trust, source) {
		t.Fatal("runtime facts came from history")
	}
	if !strings.Contains(jsonOf(t, object(t, objects, "Deployment", "oberth-controller-workflow-controller")), "quay.io/argoproj/workflow-controller:v4.0.15") || !strings.Contains(jsonOf(t, object(t, objects, "StatefulSet", "oberth-store")), "docker.io/openbao/openbao:2.7.2") {
		t.Fatal("native refs came from history")
	}
	if _, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(f.dir + "/rollback.json")
	if err != nil || !bytes.Contains(body, []byte(`"target_schema":2`)) {
		t.Fatal("target schema did not come from chart", err)
	}
}
