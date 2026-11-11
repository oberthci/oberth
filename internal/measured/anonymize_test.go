package measured

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnonymizerHashesNamesWithAPerExportSalt(t *testing.T) {
	report := Report{Repository: "github/acme/widgets", Ref: "land/secret-feature", Run: "0123456789abcdef0123456789abcdef", NewestSHA: strings.Repeat("a", 40),
		Steps: []Step{{Stage: "test", Template: "test-store", OwnerRepository: "github/acme/widgets", RunIDs: []string{"0123456789abcdef0123456789abcdef"},
			Runs: 5, Declared: &Declaration{Stage: "test", Template: "test-store", OwnerRepository: "github/acme/shared", SHA: strings.Repeat("b", 40)}}}}
	first, err := NewAnonymizer()
	if err != nil {
		t.Fatal(err)
	}
	anonymized := first.Report(report)
	raw, err := json.Marshal(anonymized)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"acme", "widgets", "shared", "secret-feature", "test-store", "0123456789abcdef", strings.Repeat("a", 40), strings.Repeat("b", 40)} {
		if strings.Contains(string(raw), name) {
			t.Fatalf("anonymized report keeps %q: %s", name, raw)
		}
	}
	step := anonymized.Steps[0]
	if step.OwnerRepository != anonymized.Repository || step.Template != step.Declared.Template || step.RunIDs[0] != anonymized.Run || step.Runs != 5 {
		t.Fatalf("tokens are not stable within one export: %+v", step)
	}
	if !strings.HasPrefix(anonymized.Repository, "repo-") || !strings.HasPrefix(step.Template, "step-") || !strings.HasPrefix(anonymized.Ref, "ref-") {
		t.Fatalf("token kinds: %s %s %s", anonymized.Repository, step.Template, anonymized.Ref)
	}
	if report.Repository != "github/acme/widgets" || report.Steps[0].Declared.Template != "test-store" {
		t.Fatal("anonymizing changed the input report")
	}
	second, err := NewAnonymizer()
	if err != nil {
		t.Fatal(err)
	}
	if second.Report(report).Repository == anonymized.Repository {
		t.Fatal("two exports share a salt")
	}
}
