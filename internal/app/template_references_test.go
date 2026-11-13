package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestArgoJobsRefusesExternalTemplateReferences(t *testing.T) {
	for _, reference := range []string{"shared/pipeline@v1", "shared-template"} {
		for _, site := range []string{"step", "dag", "hook", "workflow"} {
			t.Run(reference+"/"+site, func(t *testing.T) {
				fixture := newRaceFixture(t)
				source := "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nspec:\n  entrypoint: main\n  templates:\n  - name: main\n"
				switch site {
				case "step":
					source += "    steps:\n    - - name: call\n        templateRef: {name: REFERENCE, template: run}\n"
				case "dag":
					source += "    dag:\n      tasks:\n      - name: call\n        templateRef: {name: REFERENCE, template: run}\n"
				case "hook":
					source += "    steps:\n    - - name: call\n        template: main\n        hooks:\n          exit:\n            templateRef: {name: REFERENCE, template: run}\n"
				case "workflow":
					source = "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nspec:\n  workflowTemplateRef: {name: REFERENCE}\n"
				}
				source = strings.ReplaceAll(source, "REFERENCE", reference)
				if err := os.WriteFile(filepath.Join(fixture.sourceDir, ".oberth", "build.yaml"), []byte(source), 0600); err != nil {
					t.Fatal(err)
				}
				request := fixture.request("refused-reference", "refused-run")
				var refusal *argoworkflow.AdmissionError
				if err := fixture.jobs.CreateWorkflow(t.Context(), request); !errors.As(err, &refusal) {
					t.Fatalf("external reference was not a document refusal: %v", err)
				}
				if _, err := fixture.jobs.ResolvePipelineSource(t.Context(), request); err == nil {
					t.Fatal("external reference received admission receipt")
				}
				if _, err := fixture.jobs.PreflightRelease(t.Context(), request, []byte(source)); err == nil {
					t.Fatal("external reference passed release preflight")
				}
				if len(fixture.controller.created) != 0 {
					t.Fatal("refused source had submission effects")
				}
			})
		}
	}
}
