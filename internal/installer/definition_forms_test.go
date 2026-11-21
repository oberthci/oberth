package installer_test

import (
	"strings"
	"testing"

	definitions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestBorrowedDefinitionsPreserveAdmittedForms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field, path string
		schema      definitions.JSONSchemaProps
	}{
		{"containerSet", "spec.templates[0].containerSet.containers", definitions.JSONSchemaProps{Type: "object"}},
		{"suspend", "spec.templates[0].suspend.duration", definitions.JSONSchemaProps{Type: "object"}},
		{"resource", "spec.templates[0].resource.action", definitions.JSONSchemaProps{Type: "object"}},
		{"resource", "spec.templates[0].resource.manifest", definitions.JSONSchemaProps{Type: "object", Properties: map[string]definitions.JSONSchemaProps{"action": {Type: "string"}}}},
		{"dag", "spec.templates[0].dag.tasks[0].template", invocationSchema("dag", "template")},
		{"steps", "spec.templates[0].steps[0][0].template", invocationSchema("steps", "template")},
		{"dag", "spec.templates[0].dag.tasks[0].templateRef.name", invocationSchema("dag", "templateRef")},
		{"steps", "spec.templates[0].steps[0][0].templateRef.name", invocationSchema("steps", "templateRef")},
	} {
		t.Run(tc.path, func(t *testing.T) {
			f := setup(t)
			reader := definitionFixture(t, f, "control")
			narrowTemplateSchema(reader.Objects["workflows.argoproj.io"].Spec.Versions[0].Schema.OpenAPIV3Schema, tc.field, tc.schema)
			_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if err == nil || !strings.Contains(err.Error(), "workflows.argoproj.io") || !strings.Contains(err.Error(), "dropped paths: "+tc.path) {
				t.Fatal("admitted form pruning not explained", err)
			}
			assertNoInstallWrites(t, f)
		})
	}
}

func invocationSchema(group, dropped string) definitions.JSONSchemaProps {
	keep := true
	object := definitions.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &keep}
	item := definitions.JSONSchemaProps{Type: "object", Properties: map[string]definitions.JSONSchemaProps{"name": {Type: "string"}, "inline": object, "arguments": object, "templateRef": object, "template": {Type: "string"}}}
	if dropped == "templateRef" {
		item.Properties[dropped] = definitions.JSONSchemaProps{Type: "object"}
	} else {
		delete(item.Properties, dropped)
	}
	array := definitions.JSONSchemaProps{Type: "array", Items: &definitions.JSONSchemaPropsOrArray{Schema: &item}}
	if group == "steps" {
		return definitions.JSONSchemaProps{Type: "array", Items: &definitions.JSONSchemaPropsOrArray{Schema: &array}}
	}
	return definitions.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &keep, Properties: map[string]definitions.JSONSchemaProps{"tasks": array}}
}
