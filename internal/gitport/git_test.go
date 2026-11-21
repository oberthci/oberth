package gitport

import (
	"context"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestGitMethodSet(t *testing.T) {
	typ := reflect.TypeFor[Git]()
	cases := []struct {
		name      string
		signature reflect.Type
	}{
		{"ReadFile", reflect.TypeFor[func(context.Context, Revision, string) (File, error)]()},
		{"ResolveRef", reflect.TypeFor[func(context.Context, string, string) (Ref, error)]()},
		{"ListRefs", reflect.TypeFor[func(context.Context, string) ([]Ref, error)]()},
		{"RemoteRef", reflect.TypeFor[func(context.Context, string, string) (Ref, error)]()},
		{"PreparePromotion", reflect.TypeFor[func(context.Context, PromotionRequest) (Promotion, error)]()},
	}
	if typ.NumMethod() != len(cases) {
		t.Fatalf("method count = %d, want %d", typ.NumMethod(), len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method, found := typ.MethodByName(tc.name)
			if !found || method.Type != tc.signature {
				t.Fatalf("method = %v, want %s", method, tc.signature)
			}
		})
	}
}

func TestRevisionUsesSourceRepresentation(t *testing.T) {
	if reflect.TypeFor[Revision]() != reflect.TypeFor[sourcecontract.Revision]() {
		t.Fatal("Git revision duplicates the source revision")
	}
}
