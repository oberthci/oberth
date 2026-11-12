package argoworkflow_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// This probe is copied into the exact deployed server revision by the shell
// runner. It exercises that revision's decoder and admission, not current code.
func TestBootstrapRepositoryAdmission(t *testing.T) {
	if len(flag.Args()) != 1 {
		t.Fatal("supply the candidate repository root after -args")
	}
	paths, err := filepath.Glob(filepath.Join(flag.Args()[0], ".oberth", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	workflows := 0
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var header struct{ Kind string }
			if err := yaml.Unmarshal(source, &header); err != nil {
				t.Fatal(err)
			}
			if header.Kind == "" {
				t.Log("non-Workflow repository configuration")
				return
			}
			workflows++
			workflow, err := argoworkflow.Decode(source)
			if err == nil {
				err = argoworkflow.Admit(workflow, argoworkflow.Policy{RunnerImagePrefixes: []string{"golang:", "debian:", "aquasec/trivy:"}})
			}
			if err != nil {
				for _, refusal := range bootstrapRefusals(err) {
					t.Errorf("REFUSED .oberth/%s: %s", filepath.Base(path), refusal)
				}
				return
			}
			t.Logf("ACCEPTED .oberth/%s", filepath.Base(path))
		})
	}
	if workflows < 2 {
		t.Errorf("found %d Workflows; build and release must both be checked", workflows)
	}
}

func bootstrapRefusals(err error) []string {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var result []string
		for _, child := range joined.Unwrap() {
			result = append(result, bootstrapRefusals(child)...)
		}
		return result
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok && wrapped.Unwrap() != nil {
		return bootstrapRefusals(wrapped.Unwrap())
	}
	return []string{strings.TrimSpace(fmt.Sprint(err))}
}
