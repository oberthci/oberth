package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestReview1553SyncFilterInteger(t *testing.T) {
	raw := []byte(`{"controller":{"resources":{"requests":{"cpu":"100m","memory":"128Mi"}}},"extraObjects":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"oberth-sync-limits","namespace":"pipelines"},"data":{"deploy":"1"}},{"apiVersion":"example.test/v1","kind":"Checkpoint","metadata":{"name":"operator-checkpoint"},"spec":{"offset":9007199254740993}}]}`)
	deps := Deps{RunHelm: func(context.Context, []string) ([]byte, error) { return raw, nil }}
	args, changed, err := removeSyncLimitsValues(t.Context(), deps, "pipelines", true)
	if err != nil || !changed {
		t.Fatal(args, changed, err)
	}
	t.Logf("FILTERED ARGV: %s", args)
	body := strings.TrimPrefix(args[1], "extraObjects=")
	dec := json.NewDecoder(bytes.NewBufferString(body))
	dec.UseNumber()
	var values []map[string]any
	if err = dec.Decode(&values); err != nil {
		t.Fatal(err)
	}
	got := values[0]["spec"].(map[string]any)["offset"]
	if got != json.Number("9007199254740993") {
		t.Errorf("operator integer changed from 9007199254740993 to %v", got)
	}
}

// Inspect the complete final invocation: dependency cleanup and profile
// layering must preserve the same unrelated object through every values file.
func TestArgoUpgradeCombinedValuesPreserveExactNumbers(t *testing.T) {
	const ns = "oberth-argo"
	for _, nonroot := range []bool{false, true} {
		t.Run(map[bool]string{false: "stock", true: "nonroot"}[nonroot], func(t *testing.T) {
			stored := map[string]any{
				"controller": map[string]any{"parallelism": json.Number("2"), "priorityClassName": controlPlanePriority(Config{}), "resources": map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "128Mi"}}},
				"extraObjects": []any{
					map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": retiredSyncLimitsName, "namespace": ns}},
					map[string]any{"apiVersion": "example.test/v1", "kind": "Checkpoint", "metadata": map[string]any{"name": "operator-checkpoint"}, "spec": map[string]any{"offset": json.Number("9007199254740993"), "bounds": []any{json.Number("9223372036854775807"), json.Number("-9007199254740993")}}},
				},
			}
			raw, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			upgraded := false
			deps := Deps{Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "list":
					return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
				case "get":
					return raw, nil
				case "upgrade":
					upgraded = true
					var objects []any
					var parallelism any
					for i, arg := range args {
						if arg == "--values" {
							body, err := os.ReadFile(args[i+1])
							if err != nil {
								t.Fatal(err)
							}
							decoder := json.NewDecoder(bytes.NewReader(body))
							decoder.UseNumber()
							var values map[string]any
							if err = decoder.Decode(&values); err != nil {
								t.Fatal(err)
							}
							if value, ok := values["extraObjects"]; ok {
								objects = value.([]any)
							}
							if controller, ok := values["controller"].(map[string]any); ok {
								if value, ok := controller["parallelism"]; ok {
									parallelism = value
								}
								if controller["priorityClassName"] == controlPlanePriority(Config{}) || controller["resources"] != nil {
									t.Fatal("managed dependency scheduling survived", controller)
								}
							}
						}
						if arg == "--set-json" && strings.HasPrefix(args[i+1], "extraObjects=") {
							decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(args[i+1], "extraObjects=")))
							decoder.UseNumber()
							if err := decoder.Decode(&objects); err != nil {
								t.Fatal(err)
							}
						}
					}
					if parallelism != json.Number("2") {
						t.Fatal("stored cap changed", parallelism)
					}
					want := 1
					if nonroot {
						want = 2
					}
					if len(objects) != want {
						t.Fatal("wrong final extraObjects", objects)
					}
					checkpoint := objects[len(objects)-1].(map[string]any)
					if !reflect.DeepEqual(checkpoint, stored["extraObjects"].([]any)[1]) {
						t.Fatal("operator object changed", checkpoint)
					}
					for _, object := range objects {
						if isRetiredSyncLimitsObject(object, ns) {
							t.Fatal("retired object rendered again")
						}
					}
					if nonroot && !isNonrootProfileObject(objects[0], ns) {
						t.Fatal("profile lost", objects)
					}
				}
				return nil, nil
			}}
			cfg := Config{ArgoNamespace: ns}
			if nonroot {
				cfg.ArgoControllerProfile = argoworkflow.NonrootStaticProfile
				cfg.ArgoValuesReviewed = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
			}
			result, err := InstallArgoWorkflows(t.Context(), cfg, deps)
			if err != nil || !upgraded {
				t.Fatal(result, err, upgraded)
			}
			if nonroot && (result.Profile == nil || result.Profile.Parallelism != 2) {
				t.Fatal("stored cap lost in profile", result.Profile)
			}
		})
	}
}

func TestNonrootParallelismExactStoredNumbers(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  int
		valid bool
	}{
		{json.Number("2"), 2, true}, {float64(2), 2, true}, {2, 2, true}, {int64(2), 2, true},
		{json.Number("2.0000000000000001"), 0, false}, {json.Number("9007199254740993"), 0, false},
		{json.Number("9223372036854775808"), 0, false}, {json.Number("-1"), 0, false}, {float64(2.5), 0, false},
	} {
		got, valid := nonrootParallelism(tc.value)
		if got != tc.want || valid != tc.valid {
			t.Fatalf("%v: %d,%v", tc.value, got, valid)
		}
	}
}
