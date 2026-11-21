package strictjson

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

type location struct {
	path     string
	accepted []string
	cause    error
}

// Locate only a failed decode. Standard decoders identify the failing child;
// raw fragments provide paths without storing metadata for successful requests.
func failedLocation(data []byte, typ reflect.Type, path string, cause error) location {
	here := location{path: path, cause: cause}
	if opaqueType(typ) {
		return here
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		return objectLocation(data, typ, here)
	case reflect.Array, reflect.Slice:
		var elements []json.RawMessage
		if json.Unmarshal(data, &elements) != nil {
			return here
		}
		for index, raw := range elements {
			if err := decodeFragment(raw, typ.Elem()); err != nil {
				return failedLocation(raw, typ.Elem(), fmt.Sprintf("%s[%d]", path, index), err)
			}
		}
	case reflect.Map:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			return here
		}
		for _, key := range keys(fields) {
			if err := decodeFragment(fields[key], typ.Elem()); err != nil {
				return failedLocation(fields[key], typ.Elem(), fieldPath(path, key), err)
			}
		}
	}
	return here
}
func objectLocation(data []byte, typ reflect.Type, here location) location {
	fields := declaredFields(typ)
	for name := range fields {
		here.accepted = append(here.accepted, name)
	}
	slices.Sort(here.accepted)
	var values map[string]json.RawMessage
	if json.Unmarshal(data, &values) != nil {
		return here
	}
	for _, key := range keys(values) {
		childType := fields[key]
		if childType == nil {
			for name, candidate := range fields {
				if strings.EqualFold(name, key) {
					childType = candidate
					break
				}
			}
		}
		childPath := fieldPath(here.path, key)
		if childType == nil {
			return location{childPath, here.accepted, fmt.Errorf("json: unknown field %q", key)}
		}
		if err := decodeFragment(values[key], childType); err != nil {
			return failedLocation(values[key], childType, childPath, err)
		}
	}
	return here
}
func declaredFields(typ reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for _, field := range reflect.VisibleFields(typ) {
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" || (name == "" && field.Anonymous) {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields
}
func opaqueType(typ reflect.Type) bool {
	if typ.Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return true
	}
	if reflect.PointerTo(typ).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return true
	}
	return typ.Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) || reflect.PointerTo(typ).Implements(reflect.TypeFor[encoding.TextUnmarshaler]())
}
func decodeFragment(raw []byte, typ reflect.Type) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(reflect.New(typ).Interface())
}
func keys(values map[string]json.RawMessage) []string {
	names := make([]string, 0, len(values))
	for key := range values {
		names = append(names, key)
	}
	slices.Sort(names)
	return names
}
func fieldPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "." + name
}
