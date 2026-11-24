package client

import (
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestClientCatalogueMatchesDeclarations(t *testing.T) {
	for _, want := range apicontract.Definitions(apicontract.Bindings{}) {
		t.Run(want.Name, func(t *testing.T) {
			t.Parallel()
			got, ok := toolDefinition(want.Name)
			if !ok || !reflect.DeepEqual(got.Metadata, want.Metadata) || got.InputType != want.InputType || got.ResultType != want.ResultType || got.Bound {
				t.Fatal("cached declaration changed", want.Name)
			}
			if got.InputType.Size() > 0 && got.NewInput() == got.NewInput() {
				t.Fatal("catalogue shares request inputs", want.Name)
			}
		})
	}
	if _, ok := toolDefinition("undeclared"); ok {
		t.Fatal("unknown tool became a declaration")
	}
}

func TestClientCatalogueDoesNotRebuildPerLookup(t *testing.T) {
	allocs := testing.AllocsPerRun(25, func() { _, _ = toolDefinition("run_get") })
	t.Log("allocations per warm catalogue lookup", allocs)
	if allocs != 0 {
		t.Fatal("tool call rebuilds the catalogue", allocs)
	}
}

func BenchmarkClientToolDefinition(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := toolDefinition("run_get"); !ok {
			b.Fatal("run_get declaration missing")
		}
	}
}
