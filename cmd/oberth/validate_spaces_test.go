package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestValidateAcceptsDirectoryNamesWithSpaces(t *testing.T) {
	for _, file := range []string{"my repo/.oberth/build.yaml", "my repo/workflow.yaml"} {
		t.Run(file, func(t *testing.T) {
			app, out, diagnostic := fixture(t, fake.Bindings())
			app.directory = t.TempDir()
			name := filepath.Join(app.directory, file)
			if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, []byte(starterWorkflow()), 0600); err != nil {
				t.Fatal(err)
			}
			if code := app.run(t.Context(), []string{"validate", file, "-o", "json"}); code != 0 {
				t.Fatal(code, out.String(), diagnostic.String())
			}
			var result validationResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.OK || result.File != file {
				t.Fatal("validation lost the local filename", err, out.String())
			}
		})
	}
}
