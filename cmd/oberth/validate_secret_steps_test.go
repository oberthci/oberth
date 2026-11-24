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
	output := out.String()
	const command = "oberth check --repo '<repo>' --local '.oberth/build.yaml'"
	for _, step := range []struct{ name, path string }{{"sign", "team/signing"}, {"push", "team/registry"}} {
		need := `spec.templates["` + step.name + `"].secrets[` + step.path + `]`
		if !strings.Contains(output, need) || !strings.Contains(output, command) {
			t.Fatal("validate does not list the step's own path and check", step, output)
		}
	}
	if strings.Contains(output, `spec.templates["sign"].secrets[team/registry]`) || strings.Contains(output, `spec.templates["push"].secrets[team/signing]`) {
		t.Fatal("validate lists another step's path", output)
	}
	if strings.Contains(output, "oberth secret put") {
		t.Fatal("offline validation suggests a grant write", output)
	}
}
