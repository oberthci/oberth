package sourcecontract

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
		{Receipt{}, "Version:version Revision:revision ObjectSHA:object_sha File:file Blob:blob DocumentDigest:document_digest"},
		{Binding{}, "Template:template Artifact:artifact Path:path Descriptor:descriptor RepositoryID:repository_id Revision:revision Export:export AuthorizationDigest:authorization_digest"},
		{Export{}, "Ref:ref DefaultBranch:default_branch Tags:tags"},
		{Ref{}, "Name:ref ObjectSHA:object_sha CommitSHA:commit_sha"},
		{Revision{}, "Repository:repository Commit:commit"},
		{Descriptor{}, "Version:version Kind:kind Repository:repository,omitempty Commit:commit,omitempty"},
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
