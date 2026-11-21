package model

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
		{AgentEvent{}, "ID:id Kind:kind At:at Upstream:upstream,omitempty IssueID:issue_id,omitempty Repo:repo,omitempty RunID:run_id,omitempty PromotionID:promotion_id,omitempty Ref:ref,omitempty ObjectSHA:object_sha,omitempty CommitSHA:commit_sha Summary:summary Data:data,omitempty"},
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
