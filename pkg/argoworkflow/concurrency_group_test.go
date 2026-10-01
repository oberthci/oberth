package argoworkflow

import (
	"strings"
	"testing"
)

// groupedDocument is admissibleDocument declaring one annotation value, so the
// admission cases below differ from the minimal document in exactly the
// concurrency group.
func groupedDocument(value string) string {
	return strings.Replace(admissibleDocument, "kind: Workflow\n",
		"kind: Workflow\nmetadata:\n  annotations:\n    oberth.ci/concurrency-group: "+value+"\n", 1)
}

func TestDeclaredConcurrencyGroupAbsentIsUngrouped(t *testing.T) {
	workflow, err := Decode([]byte(admissibleDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if group, err := DeclaredConcurrencyGroup(workflow); err != nil || group != "" {
		t.Fatalf("undeclared group = %q,%v; want no group", group, err)
	}
	for _, blank := range []string{"", "  ", "\t"} {
		workflow.Annotations = map[string]string{ConcurrencyGroupAnnotation: blank}
		if group, err := DeclaredConcurrencyGroup(workflow); err != nil || group != "" {
			t.Fatalf("blank group %q = %q,%v; want no group", blank, group, err)
		}
	}
	if _, err := DeclaredConcurrencyGroup(nil); err == nil {
		t.Fatal("a nil Workflow must be refused, not read as ungrouped")
	}
}

func TestDeclaredConcurrencyGroupAcceptsTheNameGrammar(t *testing.T) {
	workflow, err := Decode([]byte(admissibleDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for declared, expected := range map[string]string{
		"terraform-state":        "terraform-state",
		"a":                      "a",
		"0":                      "0",
		"-":                      "-",
		"db-migrations-2":        "db-migrations-2",
		strings.Repeat("x", 63):  strings.Repeat("x", 63),
		"  terraform-state\t":    "terraform-state",
		"terraform-state\n":      "terraform-state",
		"state-" + "0123456789a": "state-0123456789a",
	} {
		workflow.Annotations = map[string]string{ConcurrencyGroupAnnotation: declared}
		group, err := DeclaredConcurrencyGroup(workflow)
		if err != nil || group != expected {
			t.Fatalf("group %q = %q,%v; want %q", declared, group, err, expected)
		}
		if !ValidConcurrencyGroup(expected) {
			t.Fatalf("ValidConcurrencyGroup(%q) = false for an admitted name", expected)
		}
	}
}

func TestDeclaredConcurrencyGroupRefusesEverythingOutsideTheGrammar(t *testing.T) {
	workflow, err := Decode([]byte(admissibleDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, declared := range []string{
		"Terraform-State",         // upper case is a different name, never folded
		"terraform_state",         // underscore
		"terraform.state",         // dot
		"terraform/state",         // a path separator could look like a scope
		"12/terraform-state",      // spelling another repository's scoped key
		"terraform state",         // inner whitespace
		strings.Repeat("x", 64),   // one past the bound
		"tërraform",               // non-ASCII
		"terraform-state\x00evil", // NUL
		"*",                       // glob
	} {
		workflow.Annotations = map[string]string{ConcurrencyGroupAnnotation: declared}
		group, err := DeclaredConcurrencyGroup(workflow)
		if err == nil {
			t.Fatalf("group %q admitted as %q; it is outside [a-z0-9-]{1,63}", declared, group)
		}
		if !strings.Contains(err.Error(), ConcurrencyGroupAnnotation) || !strings.Contains(err.Error(), "[a-z0-9-]") {
			t.Fatalf("refusal for %q does not name the annotation and grammar: %v", declared, err)
		}
		if ValidConcurrencyGroup(strings.TrimSpace(declared)) {
			t.Fatalf("ValidConcurrencyGroup(%q) = true for a refused name", declared)
		}
	}
}

// Admission is where `oberth validate` and the server's submission path agree:
// both reach Admit, so a malformed group fails the same way in both places.
func TestAdmitRefusesAnInvalidConcurrencyGroup(t *testing.T) {
	mustAdmit(t, groupedDocument("terraform-state"), Policy{})
	refuse(t, groupedDocument("Terraform_State"), Policy{}, ConcurrencyGroupAnnotation)
	refuse(t, groupedDocument(`"`+strings.Repeat("g", 64)+`"`), Policy{}, "invalid concurrency group")
}
