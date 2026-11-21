package runtimeconfig

import (
	"reflect"
	"strings"
	"testing"
)

// TestWireTags pins the fields and exact tags in docs/plans/README.md.
func TestWireTags(t *testing.T) {
	cases := []struct {
		value  any
		fields string
	}{
		{WorkflowDefaultsSnapshot{}, "WorkflowDefaults:workflow_defaults Digest:digest"},
		{SourcePluginConfig{}, "Endpoint:endpoint ServerName:server_name CAPEM:ca_pem ImageDigest:image_digest"},
	}
	for _, tc := range cases {
		typ := reflect.TypeOf(tc.value)
		t.Run(typ.Name(), func(t *testing.T) {
			expected := strings.Fields(tc.fields)
			if typ.NumField() != len(expected) {
				t.Fatalf("field count = %d, want %d", typ.NumField(), len(expected))
			}
			for i, entry := range expected {
				name, tag, _ := strings.Cut(entry, ":")
				field := typ.Field(i)
				if field.Name != name || field.Tag.Get("json") != tag {
					t.Errorf("field %d = %s json:%q, want %s json:%q", i, field.Name, field.Tag.Get("json"), name, tag)
				}
			}
		})
	}
}
