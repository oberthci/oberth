package app

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

// Native initialization persists substitution everywhere except templates.
// Check the final sealed spec before a reusable parent can be published.
func scheduleExpressions(spec wf.WorkflowSpec) error {
	spec.Templates = nil
	raw, err := json.Marshal(spec)
	if err != nil {
		return refusal("prepare", err)
	}
	var fields any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return refusal("prepare", err)
	}
	var findings []admissioncontract.Finding
	m := messages()["schedule"]
	paths := expressionFields(fields, "spec")
	for _, path := range paths {
		findings = append(findings, admissioncontract.Finding{Path: path, Code: "schedule", Reason: m.reason, Remedy: m.remedy})
	}
	if len(findings) > 0 {
		cause := publicerr.New(publicerr.InvalidInput, m.reason, m.remedy, nil)
		cause.Detail = strings.Join(paths, "\n")
		return &Refusal{Code: "schedule", Problems: findings, cause: cause}
	}
	return nil
}

func expressionFields(value any, path string) []string {
	var fields []string
	switch v := value.(type) {
	case string:
		if strings.Contains(v, "{{") {
			fields = append(fields, path)
		}
	case []any:
		for i, child := range v {
			fields = append(fields, expressionFields(child, fmt.Sprintf("%s[%d]", path, i))...)
		}
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(v)) {
			field := path + "." + key
			if strings.Contains(key, "{{") {
				fields = append(fields, field)
				continue
			}
			fields = append(fields, expressionFields(v[key], field)...)
		}
	}
	return fields
}
