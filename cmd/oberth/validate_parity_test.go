package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestValidateEveryPublishedWorkflowOffline(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"docs/examples", ".oberth"} {
		files, err := filepath.Glob(filepath.Join(root, dir, "*.yaml"))
		if err != nil || len(files) == 0 {
			t.Fatal("workflow fixtures missing", err)
		}
		for _, file := range files {
			// This file supplies cluster resources, not a workflow definition.
			if filepath.Base(file) == "pipeline-resources.yaml" {
				continue
			}
			file, err = filepath.Rel(root, file)
			if err != nil {
				t.Fatal(err)
			}
			t.Run(file, func(t *testing.T) { assertPublishedValidation(t, root, file) })
		}
	}
}

func assertPublishedValidation(t *testing.T, root, file string) {
	t.Helper()

	app, out, diagnostic := fixture(t, fake.Bindings())
	app.directory = root
	app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("validation contacted a server")
		return nil, nil
	})
	if code := app.run(t.Context(), []string{"validate", file, "-o", "json"}); code != 0 {
		t.Fatal(code, out.String(), diagnostic.String())
	}
	var result validationResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.OK || len(result.Refusals) != 0 || len(result.Needs) == 0 {
		t.Fatal(out.String(), err)
	}
	for _, need := range result.Needs {
		if !need.Installation || need.Code != "live" && need.Command == "" {
			t.Fatal("requirement lost its command", need)
		}
	}
	assertPublishedHumanOutput(t, root, result)
}

func assertPublishedHumanOutput(t *testing.T, root string, result validationResult) {
	t.Helper()
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.directory = root
	if code := app.run(t.Context(), []string{"validate", result.File}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	if !strings.Contains(out.String(), "Needs before the first run:") {
		t.Fatal("needs have no separate heading", out.String())
	}
	for _, need := range result.Needs {
		if need.Command != "" && !strings.Contains(out.String(), need.Command+"\n") {
			t.Fatal("printed command cannot be copied whole", need.Command, out.String())
		}
	}
}

func TestValidateTwentyRunsHaveAllThreeOrderedNativeFaults(t *testing.T) {
	body := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  templates:
  - name: main
    dag:
      tasks:
      - {name: a, template: z-first}
      - {name: b, template: a-second}
      - {name: c, template: m-third}
  - name: z-first
    container: {image: image, args: ['{{workflow.parameters.missing-one}}']}
  - name: a-second
    container: {image: image, args: ['{{workflow.parameters.missing-two}}']}
  - name: m-third
    container: {image: image, args: ['{{workflow.parameters.missing-three}}']}
`
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.directory = t.TempDir()
	if err := os.WriteFile(filepath.Join(app.directory, "workflow.yaml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	var first string
	for i := range 20 {
		out.Reset()
		diagnostic.Reset()
		if code := app.run(t.Context(), []string{"validate", "workflow.yaml", "-o", "json"}); code != 1 {
			t.Fatal("invalid workflow passed", code, out.String())
		}
		var result validationResult
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Refusals) != 3 {
			t.Fatal(out.String(), err)
		}
		for n, name := range []string{"z-first", "a-second", "m-third"} {
			if !strings.Contains(result.Refusals[n].Path, name) {
				t.Fatal("not file order", result.Refusals)
			}
		}
		if i == 0 {
			first = out.String()
		} else if out.String() != first {
			t.Fatal("validation output changed", first, out.String())
		}
	}
}
