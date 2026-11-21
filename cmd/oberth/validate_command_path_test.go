package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestValidateCommandsUseRepositoryFile(t *testing.T) {
	document := validationGrantDocument(t)
	for _, tc := range []struct{ local, selected string }{
		{"repo-a/.oberth/build.yaml", ".oberth/build.yaml"},
		{"my repo/.oberth/build.yaml", ".oberth/build.yaml"},
		{"docs/examples/build.yaml", "<file>"},
	} {
		t.Run(tc.local, func(t *testing.T) {
			app, out, diagnostic := fixture(t, fake.Bindings())
			app.directory = t.TempDir()
			name := filepath.Join(app.directory, tc.local)
			if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, document, 0600); err != nil {
				t.Fatal(err)
			}
			if code := app.run(t.Context(), []string{"validate", tc.local, "-o", "json"}); code != 0 {
				t.Fatal(code, out.String(), diagnostic.String())
			}
			var result validationResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			assertValidationCommandPaths(t, result, tc.local, tc.selected)
		})
	}
}

func validationGrantDocument(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../docs/examples/build.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w, err := argoworkflow.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	w.Spec.Templates[0].Metadata.Annotations = map[string]string{argoworkflow.SecretPathsAnnotation: "ci/token"}
	w.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec"}
	raw, err = json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertValidationCommandPaths(t *testing.T, result validationResult, local, selected string) {
	t.Helper()
	secret, live := false, false
	for _, need := range result.Needs {
		if strings.Contains(need.Command, local) {
			t.Fatal("local path escaped into remedy", need)
		}
		secret = secret || strings.Contains(need.Command, "--file '"+selected+"'")
		live = live || strings.Contains(need.Command, "--local '"+selected+"'")
	}
	if !secret || !live {
		t.Fatal("repository selection absent from commands", result.Needs)
	}
}
