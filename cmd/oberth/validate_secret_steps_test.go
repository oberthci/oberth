package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

const twoSecretStepsWorkflow = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  templates:
    - name: main
      dag:
        tasks:
          - {name: sign, template: sign}
          - {name: push, template: push, depends: sign}
    - name: sign
      metadata:
        annotations: {oberth.ci/secret-paths: team/signing}
      container: {image: example.test/build:1, command: [oberth, secretstore, exec, --, "true"]}
    - name: push
      metadata:
        annotations: {oberth.ci/secret-paths: team/registry}
      container: {image: example.test/build:1, command: [oberth, secretstore, exec, --, "true"]}
`

func TestValidateListsEachStepsDeclaredPaths(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.directory = t.TempDir()
	name := filepath.Join(app.directory, ".oberth", "build.yaml")
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(twoSecretStepsWorkflow), 0600); err != nil {
		t.Fatal(err)
	}
	if code := app.run(t.Context(), []string{"validate", ".oberth/build.yaml"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	for _, step := range []struct{ name, path string }{{"sign", "team/signing"}, {"push", "team/registry"}} {
		need := `spec.templates["` + step.name + `"].secrets[` + step.path + `]`
		command := "oberth secret put --repo '<repo>' --file '.oberth/build.yaml' --step '" + step.name + "' '" + step.path + "' < <value-file>"
		if !strings.Contains(out.String(), need) || !strings.Contains(out.String(), command) {
			t.Fatal("validate does not list the step's own path and command", step, out.String())
		}
	}
	if strings.Contains(out.String(), "--step 'sign' 'team/registry'") || strings.Contains(out.String(), "--step 'push' 'team/signing'") {
		t.Fatal("validate asks to grant a step another step's path", out.String())
	}
}
