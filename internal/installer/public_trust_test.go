package installer_test

import (
	"strings"
	"testing"
)

func TestSourceTrustHasNoUnusedPublicCAObject(t *testing.T) {
	t.Parallel()
	objects := rendered(t, nil)
	for _, o := range objects {
		if o["kind"] == "ConfigMap" && o["metadata"].(map[string]any)["name"] == "oberth-ca-public" {
			t.Fatal("unused public CA object rendered")
		}
		if strings.Contains(jsonOf(t, o), "public_ca_config_map") {
			t.Fatal("unused public CA setting rendered")
		}
	}
	source := object(t, objects, "ConfigMap", "oberth-source")["data"].(map[string]any)
	if !strings.Contains(source["source.json"].(string), "ca_pem") {
		t.Fatal("source trust missing")
	}
}
