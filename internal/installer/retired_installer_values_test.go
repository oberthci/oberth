package installer

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestRetiredInstallerCleanupMatchesHistoricalSnapshot(t *testing.T) {
	var old, cleaned map[string]any
	for _, f := range []struct {
		name string
		dst  *map[string]any
	}{{"v0.22.3-values.yaml.golden", &old}, {"v0.22.3-cleaned-values.yaml.golden", &cleaned}} {
		body, err := os.ReadFile("testdata/" + f.name)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(body, f.dst); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if !dropRetiredInstallerValues(old, &output) || !reflect.DeepEqual(old, cleaned) {
		t.Fatal("historical snapshot cleanup differs from the chart fixture")
	}
	if strings.Count(output.String(), "\n") != 1 {
		t.Fatal("cleanup must emit one note", output.String())
	}
	before := output.String()
	if dropRetiredInstallerValues(old, &output) || output.String() != before {
		t.Fatal("cleanup is not idempotent")
	}
}

func TestRetiredInstallerCleanupKeepsEveryOtherKey(t *testing.T) {
	live := map[string]any{"enabled": true, "role": "operator-role", "transit": map[string]any{"enabled": false, "mount": "old-mount", "key": "old-key", "operatorUnknown": "must-be-validated"}}
	values := map[string]any{"secretstore": live, "argo": map[string]any{"goProxy": map[string]any{"enabled": false}}, "unknownOperatorFeature": true}
	var output bytes.Buffer
	if !dropRetiredInstallerValues(values, &output) {
		t.Fatal("retired keys not found")
	}
	if live["enabled"] != true || live["role"] != "operator-role" || values["unknownOperatorFeature"] != true || !reflect.DeepEqual(live["transit"], map[string]any{"operatorUnknown": "must-be-validated"}) || values["argo"].(map[string]any)["goProxy"].(map[string]any)["enabled"] != false {
		t.Fatal("cleanup changed an unowned or live value")
	}
	if strings.Contains(output.String(), "old-key") || strings.Contains(output.String(), "old-mount") {
		t.Fatal("cleanup printed stored values")
	}
}

func TestRetiredWorkflowTTLValueIsRemovedWithoutChangingNativeOperatorDefaults(t *testing.T) {
	native := map[string]any{"secondsAfterCompletion": 0}
	argo := map[string]any{"workflowTTL": 3600, "otherOperatorValue": "kept", "workflowDefaults": map[string]any{"spec": map[string]any{"ttlStrategy": native}}}
	values := map[string]any{"argo": argo}
	var output bytes.Buffer
	if !dropRetiredInstallerValues(values, &output) {
		t.Fatal("retired server override remained")
	}
	if _, ok := argo["workflowTTL"]; ok {
		t.Fatal("retired TTL remains")
	}
	if argo["otherOperatorValue"] != "kept" || native["secondsAfterCompletion"] != 0 || !strings.Contains(output.String(), "argo.workflowTTL") {
		t.Fatal(values, output.String())
	}
}
