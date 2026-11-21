package installer_test

import (
	"strings"
	"testing"

	definitions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestBorrowedCronDefinitionPreservesPauseIntent(t *testing.T) {
	t.Parallel()
	f := setup(t)
	reader := definitionFixture(t, f, "control")
	root := reader.Objects["cronworkflows.argoproj.io"].Spec.Versions[0].Schema.OpenAPIV3Schema
	keep := true
	root.Properties["spec"] = definitions.JSONSchemaProps{Type: "object", Properties: map[string]definitions.JSONSchemaProps{
		"workflowSpec":               {Type: "object", XPreserveUnknownFields: &keep},
		"workflowMetadata":           {Type: "object", XPreserveUnknownFields: &keep},
		"schedules":                  {Type: "array", Items: &definitions.JSONSchemaPropsOrArray{Schema: &definitions.JSONSchemaProps{Type: "string"}}},
		"timezone":                   {Type: "string"},
		"startingDeadlineSeconds":    {Type: "integer"},
		"successfulJobsHistoryLimit": {Type: "integer"},
		"failedJobsHistoryLimit":     {Type: "integer"},
	}}
	_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if err == nil || !strings.Contains(err.Error(), "cronworkflows.argoproj.io") || !strings.Contains(err.Error(), "dropped paths: spec.suspend") {
		t.Fatal("scheduled pause pruning not explained", err)
	}
	assertNoInstallWrites(t, f)
}
