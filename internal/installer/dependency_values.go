package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
)

// dependencySchedulingValues removes the three managed Pod overrides from
// stored user values before Helm coalesces the upstream defaults. Resetting
// with this complete filtered map preserves unrelated operator configuration.
func dependencySchedulingValues(ctx context.Context, deps Deps, release, namespace, component, managedClass string, exists bool) (string, bool, func(), error) {
	noop := func() {}
	if !exists {
		return "", false, noop, nil
	}
	raw, err := deps.RunHelm(ctx, []string{"get", "values", release, "-n", namespace, "-o", "json"})
	if err != nil {
		return "", false, noop, fmt.Errorf("read %s values: %w", release, err)
	}
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&values); err != nil {
		return "", false, noop, fmt.Errorf("decode %s values: %w", release, err)
	}
	changed := false
	for _, item := range []struct {
		path  []string
		value string
	}{
		{[]string{component, "priorityClassName"}, managedClass},
		{[]string{component, "resources", "requests", "cpu"}, "100m"},
		{[]string{component, "resources", "requests", "memory"}, "128Mi"},
	} {
		if dependencyValueEquals(values, item.path, item.value) {
			changed = deleteDependencyValue(values, item.path) || changed
		}
	}
	if component == "controller" {
		changed = deleteDependencyValue(values, []string{component, "namespaceParallelism"}) || changed
	}
	if !changed {
		return "", false, noop, nil
	}
	file, err := os.CreateTemp("", "oberth-dependency-values-*.json")
	if err != nil {
		return "", false, noop, err
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	err = json.NewEncoder(file).Encode(values)
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return "", false, noop, err
	}
	return file.Name(), true, cleanup, nil
}
func deleteDependencyValue(values map[string]any, path []string) bool {
	if len(path) == 1 {
		if _, ok := values[path[0]]; ok {
			delete(values, path[0])
			return true
		}
		return false
	}
	child, ok := values[path[0]].(map[string]any)
	if !ok {
		return false
	}
	changed := deleteDependencyValue(child, path[1:])
	if changed && len(child) == 0 {
		delete(values, path[0])
	}
	return changed
}
func withDependencyValues(args []string, path string) []string {
	if path == "" {
		return args
	}
	args = slices.DeleteFunc(args, func(arg string) bool { return arg == "--reuse-values" })
	return append(args, "--reset-values", "--values", path)
}

func dependencyValueEquals(values map[string]any, path []string, want string) bool {
	if len(path) == 1 {
		got, ok := values[path[0]].(string)
		return ok && got == want
	}
	child, ok := values[path[0]].(map[string]any)
	return ok && dependencyValueEquals(child, path[1:], want)
}
