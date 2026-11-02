package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestDeadcodeIsRequiredByBothLintStages(t *testing.T) {
	for _, file := range []string{"build.yaml", "release.yaml"} {
		t.Run(file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", ".oberth", file))
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				Spec struct {
					Templates []struct {
						Name  string
						Steps [][]struct{ Name, Template string }
					}
				}
			}
			if err := yaml.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			stage := "lint"
			if file == "release.yaml" {
				stage = "release-lint"
			}
			found := false
			for _, tmpl := range document.Spec.Templates {
				if tmpl.Name != stage {
					continue
				}
				for _, step := range tmpl.Steps {
					for _, task := range step {
						if task.Template == "deadcode" {
							found = true
						}
					}
				}
			}
			if !found {
				t.Errorf("%s must require deadcode", stage)
			}
			if !strings.Contains(string(data), "golang.org/x/tools/cmd/deadcode@v0.49.0") {
				t.Error("missing pinned deadcode tool")
			}
		})
	}
}
