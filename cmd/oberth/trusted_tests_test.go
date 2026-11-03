package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrustedRecipesClosedPolicy(t *testing.T) {
	valid := `[{"suite_sha256":"` + strings.Repeat("a", 64) + `","guest_sha256":"` + strings.Repeat("b", 64) + `","harness_image":"registry.example/harness@sha256:` + strings.Repeat("c", 64) + `","suite_url":"https://example.org/suite","guest_url":"https://example.org/guest"}]`
	for _, test := range []struct {
		name, body string
		valid      bool
	}{{"valid", valid, true}, {"empty", "[]", false}, {"extra", strings.Replace(valid, "suite_url", "command", 1), false}, {"query", strings.Replace(valid, "https://example.org/suite", "https://example.org/suite?", 1), false}, {"mutable image", strings.Replace(valid, "@sha256:"+strings.Repeat("c", 64), ":latest", 1), false}, {"trailing", valid + "[]", false}} {
		t.Run(test.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "recipes.json")
			if err := os.WriteFile(p, []byte(test.body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := readTrustedRecipes(p)
			if (err == nil) != test.valid {
				t.Fatalf("policy result=%v", err)
			}
		})
	}
}
