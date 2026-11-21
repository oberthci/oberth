package main

import (
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScannerPreservesFactOrderAndCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.go")
	src := `package sample
const key = "input"
func action(a, b int) {
 x := key
 _ = x
 invoke(a)
 switch a { case 1: invoke(b) }
 _ = AuditActionSpec{Action: "checked"}
}
`
	if err := os.WriteFile(path, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	s := &scanner{fset: token.NewFileSet()}
	s.scan(path)
	var kinds []string
	for _, fact := range s.facts {
		kinds = append(kinds, fact.Kind)
	}
	want := []string{"value", "function", "assignment", "assignment", "call", "case", "call", "assignment", "audit", "string", "string"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("fact order = %v, want %v", kinds, want)
	}
	fn := s.facts[1]
	if !reflect.DeepEqual(fn.Args, []string{"a", "b"}) || !reflect.DeepEqual(fn.Calls, []string{"invoke", "invoke"}) {
		t.Fatalf("function facts = %+v", fn)
	}
	if s.facts[5].Calls[0] != "invoke" || s.facts[8].Values[0] != `"checked"` {
		t.Fatal("switch calls or audit actions are absent")
	}
	if s.facts[0].Line != 2 || s.facts[0].File != path || s.facts[0].Text != `key = "input"` {
		t.Fatalf("source attribution = %+v", s.facts[0])
	}
}

func TestScannerKeepsCompletedFactsAfterSliceGrowth(t *testing.T) {
	s := &scanner{fset: token.NewFileSet()}
	path := filepath.Join(t.TempDir(), "input.go")
	for range 64 {
		if err := os.WriteFile(path, []byte("package sample\nfunc action() { first(); second() }"), 0600); err != nil {
			t.Fatal(err)
		}
		s.scan(path)
	}
	for _, fact := range s.facts {
		if fact.Kind == "function" && !reflect.DeepEqual(fact.Calls, []string{"first", "second"}) {
			t.Fatalf("function lost call facts: %+v", fact)
		}
	}
}
