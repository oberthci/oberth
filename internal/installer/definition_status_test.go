package installer_test

import (
	definitions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"strings"
	"testing"
)

func TestBorrowedDefinitionPreservesControllerStatus(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"nodes", "taskResultsCompletionStatus"} {
		t.Run(field, func(t *testing.T) {
			f := setup(t)
			reader := definitionFixture(t, f, "control")
			root := reader.Objects["workflows.argoproj.io"].Spec.Versions[0].Schema.OpenAPIV3Schema
			keep := true
			properties := map[string]definitions.JSONSchemaProps{}
			for _, name := range []string{"nodes", "taskResultsCompletionStatus", "phase", "startedAt", "finishedAt", "progress", "artifactRepositoryRef", "artifactGCStatus", "conditions"} {
				if name != field {
					properties[name] = definitions.JSONSchemaProps{XPreserveUnknownFields: &keep}
				}
			}
			root.Properties["status"] = definitions.JSONSchemaProps{Type: "object", Properties: properties}
			_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if err == nil || !strings.Contains(err.Error(), "workflows.argoproj.io") || !strings.Contains(err.Error(), "dropped paths: status."+field) {
				t.Fatal("controller status pruning not explained", err)
			}
			assertNoInstallWrites(t, f)
		})
	}
}
