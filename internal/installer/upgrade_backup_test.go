package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestUpgradeLoudlyWarnsWhenFreshBackupUnverified(t *testing.T) {
	for _, live := range []string{"16", "17", "unknown"} {
		t.Run(live, func(t *testing.T) {
			var output bytes.Buffer
			deps := Deps{Output: &output, KubeClient: fake.NewClientset(), RestConfig: &rest.Config{Host: "https://cluster.example:6443"},
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					if out, ok := testUpgradeValuePreflight(args); ok {
						return out, nil
					}
					if args[0] == "list" {
						return json.Marshal([]helmRelease{{Name: "oberth", Namespace: "chosen", Status: "deployed", Chart: "oberth-0.16.18"}})
					}
					return nil, fmt.Errorf("unexpected mutating helm call: %v", args)
				},
				RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) { return []byte(live), nil },
			}
			_, err := RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.16.19", BinarySchemaVersion: 17, Namespace: "chosen", Yes: true, DryRun: true}, deps)
			if err != nil {
				t.Fatal(err)
			}
			warn := strings.Contains(output.String(), "WARNING: NO FRESH BACKUP HAS BEEN VERIFIED")
			if warn != (live != "17") {
				t.Fatalf("backup warning does not follow pending/unknown migration: %s", output.String())
			}
			if warn {
				for _, text := range []string{"backup --history-only --namespace chosen", "same cluster/context", "--yes does not prove", "Helm rollback alone"} {
					if !strings.Contains(output.String(), text) {
						t.Fatalf("missing %q: %s", text, output.String())
					}
				}
			}
		})
	}
}
