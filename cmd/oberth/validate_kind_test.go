package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestValidateInfersWorkflowKindOffline(t *testing.T) {
	for _, scenario := range []struct {
		name, path string
		declared   bool
		exit       int
	}{
		{"build", ".oberth/build.yaml", false, 1},
		{"release", ".oberth/release.yaml", false, 0},
		{"custom-ci", ".oberth/publish.yaml", false, 1},
		{"clean-build-path", "./.oberth/build.yaml", false, 1},
		{"outside-build", "build.yaml", false, 0},
		{"outside-release", "workflows/release.yaml", false, 0},
		{"prefix-boundary", ".oberth-other/build.yaml", false, 0},
		{"custom-ci-declared", ".oberth/walk.yaml", true, 0},
		{"build-declared", ".oberth/build.yaml", true, 0},
		{"release-declared", ".oberth/release.yaml", true, 1},
		{"outside-declared", "workflow.yaml", true, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			app, out, diagnostic := fixture(t, fake.Bindings())
			app.directory = t.TempDir()
			app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) {
				t.Fatal("offline validate contacted the server")
				return nil, nil
			})
			body := strings.Replace(starterWorkflow(), "        image:", "        env: [{name: INPUT, value: '{{workflow.parameters.version}}'}]\n        image:", 1)
			if scenario.declared {
				body = strings.Replace(body, "spec:\n", "spec:\n  arguments:\n    parameters: [{name: version, value: authored}]\n", 1)
			}
			file := filepath.Join(app.directory, scenario.path)
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if got := app.run(t.Context(), []string{"validate", scenario.path}); got != scenario.exit {
				t.Fatalf("exit = %d, want %d; stdout: %s; stderr: %s", got, scenario.exit, out.String(), diagnostic.String())
			}
		})
	}
}
