package installer_test

import (
	"strings"
	"testing"

	definitions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestBorrowedDefinitionsPreservePayloads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		definition, path string
		change           func(*definitions.JSONSchemaProps)
	}{
		{"workflows.argoproj.io", "spec.templates[0].", func(root *definitions.JSONSchemaProps) {
			spec := root.Properties["spec"]
			spec.Properties = map[string]definitions.JSONSchemaProps{"templates": {Type: "array", Items: &definitions.JSONSchemaPropsOrArray{Schema: &definitions.JSONSchemaProps{Type: "object"}}}}
			root.Properties["spec"] = spec
		}},
		{"workflows.argoproj.io", "spec.templates[0].inputs.artifacts[0].", func(root *definitions.JSONSchemaProps) {
			spec := root.Properties["spec"]
			keep := true
			spec.Properties = map[string]definitions.JSONSchemaProps{"templates": {Type: "array", Items: &definitions.JSONSchemaPropsOrArray{Schema: &definitions.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &keep, Properties: map[string]definitions.JSONSchemaProps{"inputs": {Type: "object", Properties: map[string]definitions.JSONSchemaProps{"artifacts": {Type: "array", Items: &definitions.JSONSchemaPropsOrArray{Schema: &definitions.JSONSchemaProps{Type: "object"}}}}}}}}}}
			root.Properties["spec"] = spec
		}},
		{"workflows.argoproj.io", "spec.volumes[0].", func(root *definitions.JSONSchemaProps) {
			spec := root.Properties["spec"]
			spec.Properties = map[string]definitions.JSONSchemaProps{"volumes": {Type: "array", Items: &definitions.JSONSchemaPropsOrArray{Schema: &definitions.JSONSchemaProps{Type: "object"}}}}
			root.Properties["spec"] = spec
		}},
		{"workflows.argoproj.io", "initContainers[0].command", func(root *definitions.JSONSchemaProps) {
			narrowTemplateSchema(root, "initContainers", definitions.JSONSchemaProps{Type: "array", Items: &definitions.JSONSchemaPropsOrArray{Schema: &definitions.JSONSchemaProps{Type: "object"}}})
		}},
		{"workflows.argoproj.io", "dropped paths: spec.templates[0].script.command", func(root *definitions.JSONSchemaProps) {
			narrowTemplateSchema(root, "script", definitions.JSONSchemaProps{Type: "object"})
		}},
		{"workflows.argoproj.io", "dropped paths: spec.templates[0].dag.tasks", func(root *definitions.JSONSchemaProps) {
			narrowTemplateSchema(root, "dag", definitions.JSONSchemaProps{Type: "object"})
		}},
		{"workflows.argoproj.io", "dropped paths: spec.templates[0].steps[0][0].arguments", func(root *definitions.JSONSchemaProps) {
			narrowTemplateSchema(root, "steps", definitions.JSONSchemaProps{Type: "array", Items: &definitions.JSONSchemaPropsOrArray{Schema: &definitions.JSONSchemaProps{Type: "array", Items: &definitions.JSONSchemaPropsOrArray{Schema: &definitions.JSONSchemaProps{Type: "object"}}}}})
		}},
		{"cronworkflows.argoproj.io", "spec.workflowSpec.", func(root *definitions.JSONSchemaProps) {
			spec := root.Properties["spec"]
			spec.Properties = map[string]definitions.JSONSchemaProps{"workflowSpec": {Type: "object"}}
			root.Properties["spec"] = spec
		}},
		{"workflowtaskresults.argoproj.io", "outputs.", func(root *definitions.JSONSchemaProps) {
			root.Properties["outputs"] = definitions.JSONSchemaProps{Type: "object"}
		}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			f := setup(t)
			reader := definitionFixture(t, f, "control")
			tc.change(reader.Objects[tc.definition].Spec.Versions[0].Schema.OpenAPIV3Schema)
			_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if err == nil || !strings.Contains(err.Error(), tc.definition) || !strings.Contains(err.Error(), tc.path) || !strings.Contains(err.Error(), "with their owner") {
				t.Fatal("pruning not explained", err)
			}
			assertNoInstallWrites(t, f)
		})
	}
}

func narrowTemplateSchema(root *definitions.JSONSchemaProps, field string, shape definitions.JSONSchemaProps) {
	keep := true
	spec := root.Properties["spec"]
	spec.Properties = map[string]definitions.JSONSchemaProps{"templates": {Type: "array", Items: &definitions.JSONSchemaPropsOrArray{Schema: &definitions.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &keep, Properties: map[string]definitions.JSONSchemaProps{field: shape}}}}}
	root.Properties["spec"] = spec
}
