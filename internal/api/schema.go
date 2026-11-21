package api

import (
	"encoding/json"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"reflect"
	"strconv"
	"strings"
	"time"
)

func schemaFor(t reflect.Type) apicontract.Schema {
	if t.Kind() == reflect.Pointer {
		return apicontract.Schema{"anyOf": []apicontract.Schema{schemaFor(t.Elem()), {"type": "null"}}}
	}
	if t == reflect.TypeFor[json.RawMessage]() {
		return apicontract.Schema{}
	}
	if t == reflect.TypeFor[publicerr.Error]() {
		return publicErrorSchema()
	}
	if t == reflect.TypeFor[time.Time]() {
		return apicontract.Schema{"type": "string", "format": "date-time"}
	}
	switch t.Kind() {
	case reflect.Struct:
		return structSchema(t)
	case reflect.String:
		return apicontract.Schema{"type": "string"}
	case reflect.Bool:
		return apicontract.Schema{"type": "boolean"}
	case reflect.Int, reflect.Int64, reflect.Uint16:
		return apicontract.Schema{"type": "integer"}
	case reflect.Slice, reflect.Array:
		return apicontract.Schema{"type": "array", "items": schemaFor(t.Elem())}
	case reflect.Map:
		return apicontract.Schema{"type": "object", "additionalProperties": schemaFor(t.Elem())}
	}
	return apicontract.Schema{}
}
func structSchema(t reflect.Type) apicontract.Schema {
	properties := apicontract.Schema{}
	required := []string{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous {
			s := schemaFor(f.Type)
			for k, v := range s["properties"].(apicontract.Schema) {
				properties[k] = v
			}
			required = append(required, s["required"].([]string)...)
			continue
		}
		key := strings.Split(f.Tag.Get("json"), ",")[0]
		if key == "" || key == "-" {
			continue
		}
		property := fieldSchema(f)
		properties[key] = property
		if f.Tag.Get("required") == "true" || !strings.Contains(f.Tag.Get("json"), ",omitempty") && !strings.Contains(f.Tag.Get("json"), ",omitzero") {
			required = append(required, key)
		}
	}
	return apicontract.Schema{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func schemaForType[T any]() apicontract.Schema { return schemaFor(reflect.TypeFor[T]()) }

func publicErrorSchema() apicontract.Schema {
	properties := apicontract.Schema{}
	for _, name := range []string{"id", "code", "reason", "remedy", "detail"} {
		properties[name] = apicontract.Schema{"type": "string"}
	}
	properties["context"] = apicontract.Schema{"type": "array", "items": apicontract.Schema{"type": "string"}}
	return apicontract.Schema{"type": "object", "properties": properties, "required": []string{"code", "reason", "remedy"}, "additionalProperties": false}
}

func fieldSchema(f reflect.StructField) apicontract.Schema {
	typ := f.Type
	if f.Tag.Get("required") == "true" && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	property := schemaFor(typ)
	if f.Tag.Get("required") == "true" && f.Type.Kind() == reflect.String {
		property["minLength"] = 1
	}
	if desc := f.Tag.Get("desc"); desc != "" {
		property["description"] = desc
	}
	for _, tag := range []string{"min", "max", "maxItems", "minItems", "maxBytes", "default"} {
		text := f.Tag.Get(tag)
		if text == "" {
			continue
		}
		name := schemaTag(tag)
		if n, err := strconv.Atoi(text); err == nil {
			property[name] = n
		} else {
			property[name] = text
		}
	}
	return property
}
func schemaTag(tag string) string {
	switch tag {
	case "min":
		return "minimum"
	case "max":
		return "maximum"
	case "maxBytes":
		return "maxLength"
	}
	return tag
}
