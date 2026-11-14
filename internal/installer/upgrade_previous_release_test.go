package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

// The stored user document follows OberthHelmArgs/argoOberthHelmValues at
// v0.22.7, including identity lists, public CA settings and explicit overrides.
// Old computed chart defaults are intentionally absent.
func TestUpgradePreviousReleaseStoredValuesAndDryRunSchema(t *testing.T) {
	requireHelmForUpgradeTest(t)
	body, err := os.ReadFile("testdata/v0.22.7-stored-config.json")
	if err != nil {
		t.Fatal(err)
	}
	chart, err := filepath.Abs("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			var stored map[string]any
			if err := json.Unmarshal(body, &stored); err != nil {
				t.Fatal(err)
			}
			if unknown {
				stored["unknownOperatorValue"] = "must-be-refused"
			}
			raw, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			renders := 0
			deps := Deps{Output: &output, KubeClient: argoSecurityTestClient(), RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"}}
			deps.Admin = idleAdminFunc(func(_ context.Context, action string, _, into any) error {
				if action != "version" {
					t.Fatalf("dry run attempted administration action %s", action)
				}
				return fixtureDecode(map[string]any{"version": "v0.22.7", "schema": 73}, into)
			})
			deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
				if out, ok := testUpgradeArgoSecurityPreflight(args); ok {
					return out, nil
				}
				switch args[0] {
				case "list":
					return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.22.7"}]`), nil
				case "history":
					return []byte(`[{"revision":1,"status":"deployed"}]`), nil
				case "get":
					if out, ok := testUpgradeArgoSecurityPreflight(args); ok {
						return out, nil
					}
					return bytes.Clone(raw), nil
				case "upgrade":
					if !slices.Contains(args, "--dry-run=server") || !slices.Contains(args, "--no-hooks") {
						t.Fatalf("upgrade preview attempted a write: %v", args)
					}
					return json.Marshal(map[string]any{"config": stored, "chart": map[string]any{"values": map[string]any{}}})
				case "version", "show", "template":
					if args[0] == "template" {
						renders++
					}
					out, err := exec.CommandContext(ctx, "helm", args...).CombinedOutput()
					if err != nil {
						return nil, fmt.Errorf("Helm: %w: %s", err, out)
					}
					return out, nil
				default:
					t.Fatalf("upgrade preflight attempted a write: %v", args)
					return nil, nil
				}
			}
			cfg := UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.22.8", ChartOverride: chart, NoBackup: true, DryRun: true}
			_, err = RunUpgrade(t.Context(), cfg, deps)
			if unknown {
				var schema *ErrSchemaValidation
				if !errors.As(err, &schema) || !strings.Contains(err.Error(), "unknownOperatorValue") || renders != 1 {
					t.Fatal("dry run failed to validate stored configuration", err, renders)
				}
				return
			}
			if err != nil || renders != 1 || !strings.Contains(output.String(), "No cluster changes were made") {
				t.Fatal(err, renders, output.String())
			}
			image, _, err := resolveChartImageRef(t.Context(), deps, chart, cfg.BinaryVersion, true)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := prepareUpgradeValues(t.Context(), cfg, deps, chart, image)
			if err != nil {
				t.Fatal(err)
			}
			saved, _ := json.Marshal(prepared.values)
			var roundTrip map[string]any
			if err := json.Unmarshal(saved, &roundTrip); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored, roundTrip) {
				t.Fatal("upgrade changed retained stored configuration")
			}
			if strings.Contains(output.String(), "Omitted retired") {
				t.Fatal("previous release contains no retired installer keys")
			}
		})
	}
}
