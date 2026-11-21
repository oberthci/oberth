package admissioncontract

import (
	"bytes"
	"reflect"
	"testing"
)

func TestTemplateOrderingUsesEveryIdentityField(t *testing.T) {
	for _, tc := range []struct {
		name        string
		left, right TemplateSnapshot
	}{
		{"namespace", TemplateSnapshot{Namespace: "a"}, TemplateSnapshot{Namespace: "b"}},
		{"name", TemplateSnapshot{Namespace: "same", Name: "a"}, TemplateSnapshot{Namespace: "same", Name: "b"}},
		{"uid", TemplateSnapshot{Namespace: "same", Name: "same", UID: "a"}, TemplateSnapshot{Namespace: "same", Name: "same", UID: "b"}},
		{"version", TemplateSnapshot{Namespace: "same", Name: "same", UID: "same", ResourceVersion: "1"}, TemplateSnapshot{Namespace: "same", Name: "same", UID: "same", ResourceVersion: "2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := preparedFixture()
			view.Templates = []TemplateSnapshot{tc.right, tc.left}
			sealed := mustSeal(t, view)
			if !reflect.DeepEqual(view.Templates, []TemplateSnapshot{tc.right, tc.left}) {
				t.Fatal("Seal sorted the producer's slice")
			}
			if !reflect.DeepEqual(sealed.View().Templates, []TemplateSnapshot{tc.left, tc.right}) {
				t.Fatal("snapshot identity order is incomplete")
			}
			first, err := sealed.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			view.Templates = []TemplateSnapshot{tc.left, tc.right}
			other := mustSeal(t, view)
			second, err := other.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("initial snapshot order changed sealed bytes")
			}
		})
	}
}
