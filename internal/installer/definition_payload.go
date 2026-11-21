package installer

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"reflect"
	"strings"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	definitions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apimachinery/pkg/runtime"
)

// The execution fixture pins these payloads to the native submission shapes.
//
//go:embed testdata/*-payload.json
var definitionPayloads embed.FS

func definitionPayload(validation *definitions.CustomResourceValidation, kind string) string {
	file := ""
	switch kind {
	case "Workflow":
		file = "workflow*"
	case "CronWorkflow":
		file = "cron-workflow*"
	case "WorkflowTaskResult":
		file = "task-result"
	default:
		return ""
	}
	if validation == nil || validation.OpenAPIV3Schema == nil {
		return "schema missing"
	}
	var internal apiext.JSONSchemaProps
	if err := definitions.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(validation.OpenAPIV3Schema, &internal, nil); err != nil {
		return fmt.Sprint(err)
	}
	schema, err := structuralschema.NewStructural(&internal)
	if err != nil {
		return fmt.Sprint(err)
	}
	files, err := fs.Glob(definitionPayloads, "testdata/"+file+"-payload.json")
	if err != nil {
		return fmt.Sprint(err)
	}
	if len(files) == 0 {
		return "payload missing"
	}
	for _, name := range files {
		if detail := pruneDefinitionFile(schema, name); detail != "" {
			return detail
		}
	}
	return ""
}

func pruneDefinitionFile(schema *structuralschema.Structural, name string) string {
	raw, err := definitionPayloads.ReadFile(name)
	if err != nil {
		return fmt.Sprint(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(raw, &payload); err != nil {
		return fmt.Sprint(err)
	}
	before := runtime.DeepCopyJSON(payload)
	dropped := pruning.PruneWithOptions(payload, schema, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
	if !reflect.DeepEqual(before, payload) {
		return "dropped paths: " + strings.Join(dropped, ", ")
	}
	return ""
}
