package argoworkflow

import (
	"encoding/json"
	"fmt"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"reflect"
	"strings"
)

// Native JSON spelling is exact, including inside native custom codecs.
// Typed extension maps keep their keys; scalar codecs own scalar validation.
func exactNativeFields(raw []byte, typ reflect.Type, path string) []admissioncontract.Finding {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[wfv1.ParallelSteps]() {
		typ = reflect.TypeFor[[]wfv1.WorkflowStep]()
	}
	if reflect.PointerTo(typ).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return nativeScalarType(raw, typ, path)
	}
	switch typ.Kind() {
	case reflect.Struct:
		return exactNativeObject(raw, typ, path)
	case reflect.Slice, reflect.Array:
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return []admissioncontract.Finding{finding(path, "field_type")}
		}
		var findings []admissioncontract.Finding
		for i, item := range items {
			findings = append(findings, exactNativeFields(item, typ.Elem(), fmt.Sprintf("%s[%d]", path, i))...)
		}
		return findings
	case reflect.Map:
		return exactNativeMap(raw, typ.Elem(), path)
	}
	return nativeScalarType(raw, typ, path)
}
func exactNativeObject(raw []byte, typ reflect.Type, path string) []admissioncontract.Finding {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return []admissioncontract.Finding{finding(path, "field_type")}
	}
	fields := nativeFields(typ)
	var findings []admissioncontract.Finding
	for _, key := range sortedKeys(object) {
		at := fieldPath(path, key)
		field, ok := fields[key]
		if !ok {
			findings = append(findings, finding(at, "unknown_field"))
			continue
		}
		findings = append(findings, exactNativeFields(object[key], field, at)...)
	}
	return findings
}
func fieldPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}
func nativeFields(typ reflect.Type) map[string]reflect.Type {
	result := map[string]reflect.Type{}
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" || inlineNativeFields(field, name, result) {
			continue
		}
		if name == "" {
			name = field.Name
		}
		result[name] = field.Type
	}
	return result
}
func exactNativeMap(raw []byte, typ reflect.Type, path string) []admissioncontract.Finding {
	var entries map[string]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return []admissioncontract.Finding{finding(path, "field_type")}
	}
	var findings []admissioncontract.Finding
	for _, key := range sortedKeys(entries) {
		findings = append(findings, exactNativeFields(entries[key], typ, fmt.Sprintf("%s[%q]", path, key))...)
	}
	return findings
}
func inlineNativeFields(field reflect.StructField, name string, result map[string]reflect.Type) bool {
	if !field.Anonymous || name != "" {
		return false
	}
	nested := field.Type
	for nested.Kind() == reflect.Pointer {
		nested = nested.Elem()
	}
	if nested.Kind() != reflect.Struct {
		return false
	}
	for key, value := range nativeFields(nested) {
		result[key] = value
	}
	return true
}

func nativeScalarType(raw []byte, typ reflect.Type, path string) []admissioncontract.Finding {
	if json.Unmarshal(raw, reflect.New(typ).Interface()) != nil {
		return []admissioncontract.Finding{finding(path, "field_type")}
	}
	return nil
}
