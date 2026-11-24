package installer

import (
	"fmt"
	"reflect"
)

func egressMode(values map[string]any) string {
	if settingText(values, "policies", "egress") != "on" {
		return "off"
	}
	network, _ := values["egressNetwork"].(map[string]any)
	if network["version"] == nil {
		return "link-local filter"
	}
	if settingText(values, "policies", "internet") == "on" {
		return "internet open, internal closed"
	}
	return "explicit destinations"
}

func reviewEgress(p *Plan, values map[string]any) []string {
	next := egressMode(values)
	before := egressMode(p.history.Deployed.Values)
	if p.history.Deployed.Number > 0 && before != next {
		next = before + " -> " + next
	}
	lines := []string{"Run egress: " + next + " (--no-egress)"}
	policy, _ := values["policies"].(map[string]any)
	previous, _ := p.history.Deployed.Values["policies"].(map[string]any)
	entries, _ := policy["allow"].([]any)
	old, _ := previous["allow"].([]any)
	if p.history.Deployed.Number > 0 && (len(entries) != 0 || len(old) != 0) && !reflect.DeepEqual(entries, old) {
		lines = append(lines, fmt.Sprintf("Egress allowances: %d -> %d; review the destinations below.", len(old), len(entries)))
	}
	for _, entry := range entries {
		value, _ := entry.(map[string]any)
		lines = append(lines, fmt.Sprintf("Egress allow: %v %v/%v", value["cidr"], value["protocol"], value["port"]))
	}
	return lines
}
