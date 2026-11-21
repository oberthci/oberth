package api_test

import (
	"strings"
	"testing"
)

func TestSecondNestedArrayBounds(t *testing.T) {
	for _, count := range []int{32, 33} {
		files := []any{}
		kinds := []any{}
		for i := 0; i < count; i++ {
			files = append(files, map[string]string{"path": "build.yaml", "content": "name: build"})
			kinds = append(kinds, map[string]any{"api_version": "v1", "kind": "Pod", "actions": []string{"get"}})
		}
		f := setup(t, nil)
		for _, tc := range []struct {
			name string
			args any
		}{
			{"run_check", map[string]any{"repo": "widget", "files": files}},
			{"workflow_capability_grant", map[string]any{"repo": "widget", "capability": map[string]any{"namespace": "execution", "service_account": "runner", "resource_kinds": kinds}}},
		} {
			w := rpc(f, tc.name, tc.args)
			if strings.Contains(w.Body.String(), `"isError":true`) != (count == 33) {
				t.Fatal("nested array boundary", tc.name, count, w.Body.String())
			}
		}
	}
}
func TestSecondEventFiltersHaveOneShapeBound(t *testing.T) {
	for _, tool := range []string{"events_wait"} {
		for _, key := range []string{"repos", "kinds", "refs_glob"} {
			f := setup(t, nil)
			initialize(f)
			values := make([]string, 32)
			for i := range values {
				values[i] = "widget"
			}
			if key == "kinds" {
				for i := range values {
					values[i] = "run.updated"
				}
			}
			assertSuccess(t, rpc(f, tool, map[string]any{key: values}))
			if w := rpc(f, tool, map[string]any{key: append(values, "widget")}); !strings.Contains(w.Body.String(), `"isError":true`) {
				t.Fatal("event filter limit bypassed", tool, key)
			}
		}
	}
	f := setup(t, nil)
	if w := rpc(f, "events_wait", map[string]any{"repos": []string{"../widget"}}); !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("invalid event repository reached service")
	}
}

func TestCapabilityActionCountBoundary(t *testing.T) {
	for _, count := range []int{4, 5} {
		actions := make([]string, count)
		for i := range actions {
			actions[i] = "get"
		}
		args := map[string]any{"repo": "widget", "capability": map[string]any{"namespace": "execution", "service_account": "runner", "resource_kinds": []any{map[string]any{"api_version": "v1", "kind": "Pod", "actions": actions}}}}
		w := rpc(setup(t, nil), "workflow_capability_grant", args)
		if strings.Contains(w.Body.String(), `"isError":true`) != (count == 5) {
			t.Fatal("capability action count", count, w.Body.String())
		}
	}
}
