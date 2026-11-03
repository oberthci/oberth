package argoworkflow

import (
	"reflect"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// FileTrigger is the inverse of TriggerFile.
func FileTrigger(file string) (pipeline.Trigger, bool) {
	switch strings.TrimSpace(file) {
	case BuildFile:
		return pipeline.TriggerCI, true
	case ReleaseFile:
		return pipeline.TriggerRelease, true
	default:
		return "", false
	}
}

// IdentityFieldPaths enumerates every distinct type-graph path at which the
// upstream schema exposes an identity field. It exists so an Argo upgrade that
// widens the attack surface is visible in a test diff rather than only in the
// normalizer's behavior.
func IdentityFieldPaths() []string {
	return identityFieldPaths(reflect.TypeOf(wfv1.WorkflowSpec{}), "WorkflowSpec", map[reflect.Type]bool{}, 0)
}

func identityFieldPaths(walked reflect.Type, path string, visiting map[reflect.Type]bool, depth int) []string {
	if depth > 12 {
		return nil
	}
	for walked.Kind() == reflect.Pointer || walked.Kind() == reflect.Slice ||
		walked.Kind() == reflect.Array || walked.Kind() == reflect.Map {
		walked = walked.Elem()
	}
	if walked.Kind() != reflect.Struct || visiting[walked] {
		return nil
	}
	visiting[walked] = true
	defer delete(visiting, walked)
	var paths []string
	for index := range walked.NumField() {
		field := walked.Field(index)
		if !field.IsExported() {
			continue
		}
		next := path + "." + field.Name
		if field.Name == serviceAccountField || field.Name == automountField {
			paths = append(paths, next)
			continue
		}
		paths = append(paths, identityFieldPaths(field.Type, next, visiting, depth+1)...)
	}
	return paths
}
