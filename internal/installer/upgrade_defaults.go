package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// v0.21.0 changed chart defaults that an existing release, upgraded with
// --reset-then-reuse-values, would otherwise pick up silently:
//
//   - retention.logs.budgetBytes 0 -> 2 GiB: the next sweep would delete the
//     oldest step logs above the budget, irreversibly (logs are not in a
//     backup). The previously rendered value is kept unless the operator
//     sets one.

// changedDefaultsPlan is what reconcileChangedDefaults decided.
type changedDefaultsPlan struct {
	args  []string // Helm flags, placed before the operator's --values/--set
	notes []string // one line per decision, printed before the first write
}

// reconcileChangedDefaults reads the deployed release and the target chart
// defaults and returns the pins above. It reads only.
func reconcileChangedDefaults(ctx context.Context, cfg UpgradeConfig, deps Deps, revision string, targetValuesYAML []byte) (changedDefaultsPlan, error) {
	var plan changedDefaultsPlan
	// The deployed revision, as every other upgrade read: a failed later
	// revision must not decide what the upgrade keeps.
	userRaw, err := deps.RunHelm(ctx, []string{"get", "values", "oberth", "-n", cfg.Namespace, "--revision", revision, "-o", "json"})
	if err != nil {
		return plan, fmt.Errorf("read stored values: %w", err)
	}
	allRaw, err := deps.RunHelm(ctx, []string{"get", "values", "oberth", "-n", cfg.Namespace, "--revision", revision, "--all", "-o", "json"})
	if err != nil {
		return plan, fmt.Errorf("read rendered values: %w", err)
	}
	var user, all, target map[string]any
	if err := json.Unmarshal(nonEmptyJSON(userRaw), &user); err != nil {
		return plan, fmt.Errorf("decode stored values: %w", err)
	}
	if err := json.Unmarshal(nonEmptyJSON(allRaw), &all); err != nil {
		return plan, fmt.Errorf("decode rendered values: %w", err)
	}
	if err := yaml.Unmarshal(targetValuesYAML, &target); err != nil {
		return plan, fmt.Errorf("decode target chart values: %w", err)
	}
	operatorSets, err := operatorSetKeys(cfg)
	if err != nil {
		return plan, err
	}
	chosen := func(key string) bool {
		_, stored := dottedValue(user, key)
		return stored || operatorSets(key)
	}

	const budget = "retention.logs.budgetBytes"
	if !chosen(budget) {
		previous := int64Value(dottedValue(all, budget))
		if next := int64Value(dottedValue(target, budget)); next != previous {
			plan.args = append(plan.args, "--set", fmt.Sprintf("%s=%d", budget, previous))
			plan.notes = append(plan.notes, fmt.Sprintf("Keeping %s=%d (this release's new default is %d, which would delete the oldest step logs above it); "+
				"adopt it with --set %s=%d", budget, previous, next, budget, next))
		}
	}

	return plan, nil
}

// operatorSetKeys reports whether the operator's --set or --values name a key
// (or a parent or child of it).
func operatorSetKeys(cfg UpgradeConfig) (func(string) bool, error) {
	paths := map[string]bool{}
	for _, kv := range cfg.UserSet {
		key, err := upgradeSetKey(kv)
		if err != nil {
			return nil, err
		}
		paths[key] = true
	}
	for _, path := range cfg.UserValues {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator's own --values file.
		if err != nil {
			return nil, fmt.Errorf("read values file %s: %w", path, err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(raw, &values); err != nil {
			return nil, fmt.Errorf("parse values file %s: %w", path, err)
		}
		collectLeafKeyPaths(values, "", paths)
	}
	return func(key string) bool { return pathsCover(paths, key) }, nil
}

func pathsCover(paths map[string]bool, key string) bool {
	for path := range paths {
		if path == key || strings.HasPrefix(path, key+".") || strings.HasPrefix(key, path+".") {
			return true
		}
	}
	return false
}

func dottedValue(values map[string]any, key string) (any, bool) {
	var current any = values
	for _, part := range strings.Split(key, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		if current, ok = object[part]; !ok {
			return nil, false
		}
	}
	return current, true
}

func int64Value(value any, _ bool) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case int64:
		return number
	case int:
		return int64(number)
	case string:
		parsed, _ := strconv.ParseInt(number, 10, 64)
		return parsed
	}
	return 0
}

func nonEmptyJSON(raw []byte) []byte {
	if len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return []byte("{}")
	}
	return raw
}

func (plan changedDefaultsPlan) print(w io.Writer) {
	for _, note := range plan.notes {
		_, _ = fmt.Fprintln(w, note)
	}
}
