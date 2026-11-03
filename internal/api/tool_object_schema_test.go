package api

import "testing"

func TestEveryToolInputSchemaIsPlainObject(t *testing.T) {
	for _, tool := range toolDefinitions() {
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Fatalf("%s: expected object input schema", tool["name"])
		}
		for _, keyword := range []string{"anyOf", "oneOf", "allOf", "$ref", "not"} {
			if _, ok := schema[keyword]; ok {
				t.Errorf("%s: top-level %s breaks tool clients", tool["name"], keyword)
			}
		}
		if _, ok := schema["properties"].(map[string]any); !ok {
			t.Errorf("%s: missing properties", tool["name"])
		}
	}
}
