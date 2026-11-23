package admissioncontract

import (
	"context"
	"encoding/json"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestHookSignatures(t *testing.T) {
	hooks := reflect.TypeFor[PrepareHooks]()
	cases := []struct {
		name      string
		signature reflect.Type
	}{
		{"OfflineValidation", reflect.TypeFor[bool]()},
		{"LoginAudience", reflect.TypeFor[string]()},
		{"ReleaseFacts", reflect.TypeFor[func(context.Context) (ReleaseFacts, error)]()},
		{"PrepareSources", reflect.TypeFor[func(context.Context, []sourcecontract.Descriptor)]()},
		{"BindSource", reflect.TypeFor[func(context.Context, sourcecontract.Descriptor) (sourcecontract.Binding, error)]()},
		{"TransformNative", reflect.TypeFor[func(context.Context, *wfv1.Workflow, TransformInput) error]()},
		{"VerifyIdentities", reflect.TypeFor[func(context.Context, IdentityRequest) (IdentityEvidence, error)]()},
	}
	if hooks.NumField() != len(cases) {
		t.Fatalf("hook count = %d, want %d", hooks.NumField(), len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			field, found := hooks.FieldByName(tc.name)
			if !found || field.Type != tc.signature || field.Tag != "" {
				t.Fatalf("hook = %v, want %s", field, tc.signature)
			}
		})
	}
}

func TestPreparedStateIsPrivate(t *testing.T) {
	if _, ok := any(new(PreparedAdmission)).(json.Unmarshaler); ok {
		t.Fatal("wire JSON can construct private prepared state")
	}
	typ := reflect.TypeFor[PreparedAdmission]()
	for i := range typ.NumField() {
		if typ.Field(i).IsExported() {
			t.Errorf("prepared state exposes field %s", typ.Field(i).Name)
		}
	}
}
