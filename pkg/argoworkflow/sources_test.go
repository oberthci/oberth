package argoworkflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPinnedSourcesDeclaration(t *testing.T) {
	source := "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nsources:\n- repository: forge/acme/producer\n  commit: \"" + strings.Repeat("a", 40) + "\"\n  path: /work/sources/producer\nspec:\n  entrypoint: main\n  templates:\n  - name: main\n    container:\n      image: example.invalid/test:1\n      command: [true]\n"
	// Decoding is a real prerequisite: an unknown sources field used to fail
	// before the server could authorize and materialize an exact checkout.
	if _, err := Decode([]byte(source)); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedSourcesRejectMalformedDeclarations(t *testing.T) {
	good := PinnedSource{Repository: "forge/acme/producer", Commit: strings.Repeat("a", 40), Path: "/work/sources/producer"}
	for _, mode := range []string{"branch", "tag", "short", "url", "alias", "traversal", "reserved", "nested", "duplicate", "too-many", "unknown-field", "trailing-json", "duplicate-fields"} {
		t.Run(mode, func(t *testing.T) {
			source := good
			switch mode {
			case "branch":
				source.Commit = "main"
			case "tag":
				source.Commit = "v1.0"
			case "short":
				source.Commit = "abcdefab"
			case "url":
				source.Repository = "https://example.invalid/repo"
			case "alias":
				source.Repository = "acme/producer"
			case "traversal":
				source.Path = "/work/sources/../src"
			case "reserved":
				source.Path = "/work/src"
			case "nested":
				source.Path += "/nested"
			}
			sources := []PinnedSource{source}
			if mode == "duplicate" {
				sources = append(sources, source)
			}
			if mode == "too-many" {
				for i := 0; i < MaxPinnedSources; i++ {
					sources = append(sources, source)
				}
			}
			body, _ := json.Marshal(sources)
			if mode == "unknown-field" {
				body = []byte(strings.Replace(string(body), `"path":`, `"credential":"never", "path":`, 1))
			}
			if mode == "trailing-json" {
				if _, err := decodePinnedSources(append(body, []byte(" []")...)); err == nil {
					t.Fatal("trailing JSON accepted")
				}
				return
			}
			if mode == "duplicate-fields" {
				body = []byte(strings.Replace(string(body), `"path":`, `"path":"/work/sources/other", "path":`, 1))
			}
			workflow := []byte("apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nsources: " + string(body) + "\nspec: {}\n")
			if _, err := Decode(workflow); err == nil {
				t.Fatal("invalid source declaration accepted")
			}
		})
	}
}

func TestPinnedSourcesAnnotationRejectsAmbiguousJSON(t *testing.T) {
	good := `[{"repository":"forge/acme/producer","commit":"` + strings.Repeat("a", 40) + `","path":"/work/sources/producer"}]`
	for _, key := range []string{"repository", "commit", "path"} {
		t.Run(key, func(t *testing.T) {
			body := strings.Replace(good, `"`+key+`":`, `"`+key+`":"ignored","`+key+`":`, 1)
			workflow := map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Workflow", "metadata": map[string]any{"annotations": map[string]string{SourcesAnnotation: body}}, "spec": map[string]any{}}
			raw, _ := json.Marshal(workflow)
			if _, err := Decode(raw); err == nil {
				t.Fatal("duplicate annotation key accepted")
			}
		})
	}
}
