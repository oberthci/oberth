package argoworkflow

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/oberthci/oberth/internal/admissioncontract"
)

func (g *graph) parameterReferences() {
	raw, _ := json.Marshal(g.workflow)
	pattern := regexp.MustCompile(`\{\{\s*workflow\.parameters\.(oberth-[a-z-]+)\s*\}\}`)
	catalogue := parameterMessages()
	for _, p := range g.stampedParameters(g.request) {
		if p.present {
			delete(catalogue, p.name)
		}
	}
	walkStrings(raw, func(path, value string) string {
		for _, match := range pattern.FindAllStringSubmatch(value, -1) {
			if _, exists := catalogue[match[1]]; exists {
				g.refuse(path, match[1])
			}
		}
		return value
	})
}

// JSON traversal keeps native field spelling and template names in diagnostics.
// It is also used to replace a reported unresolved value in diagnostic copies.
func walkStrings(raw []byte, visit func(string, string) string) []byte {
	var value any
	_ = json.Unmarshal(raw, &value)
	value = visitStrings(value, "", visit)
	result, _ := json.Marshal(value)
	return result
}

func visitStrings(value any, path string, visit func(string, string) string) any {
	switch v := value.(type) {
	case string:
		return visit(path, v)
	case []any:
		for i, item := range v {
			where := fmt.Sprintf("%s[%d]", path, i)
			if obj, ok := item.(map[string]any); ok && strings.HasSuffix(path, ".templates") {
				where = fmt.Sprintf("%s[%q]", path, stringField(obj, "name"))
			}
			v[i] = visitStrings(item, where, visit)
		}
	case map[string]any:
		for _, key := range sortedKeys(v) {
			v[key] = visitStrings(v[key], fieldPath(path, key), visit)
		}
	}
	return value
}

func unresolvedFinding(path, expression string) admissioncontract.Finding {
	name := strings.TrimPrefix(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(expression, "{{"), "}}")), "workflow.parameters.")
	if _, exists := messages()[name]; exists {
		return finding(path, name)
	}
	return finding(path, "native")
}
