package api_test

import "fmt"

func secondSchemaErrors(s map[string]any, v any, path string) []string {
	if variants, ok := s["anyOf"].([]any); ok {
		for _, a := range variants {
			if len(secondSchemaErrors(a.(map[string]any), v, path)) == 0 {
				return nil
			}
		}
		return []string{path + ": no anyOf alternative matches"}
	}
	typ, _ := s["type"].(string)
	switch typ {
	case "object":
		return secondObjectErrors(s, v, path)
	case "array":
		return secondArrayErrors(s, v, path)
	default:
		if !secondPrimitive(typ, v) {
			return []string{fmt.Sprintf("%s: want %s, got %T", path, typ, v)}
		}
	}
	return nil
}
func secondObjectErrors(s map[string]any, v any, path string) []string {
	obj, ok := v.(map[string]any)
	if !ok {
		return []string{path + ": expected object"}
	}
	props, _ := s["properties"].(map[string]any)
	var out []string
	if req, ok := s["required"].([]any); ok {
		for _, k := range req {
			if _, ok := obj[k.(string)]; !ok {
				out = append(out, path+"."+k.(string)+": missing")
			}
		}
	}
	for k, val := range obj {
		property, ok := props[k].(map[string]any)
		if !ok {
			property, _ = s["additionalProperties"].(map[string]any)
		}
		if property != nil {
			out = append(out, secondSchemaErrors(property, val, path+"."+k)...)
		} else if s["additionalProperties"] == false {
			out = append(out, path+"."+k+": undeclared")
		}
	}
	return out
}
func secondArrayErrors(s map[string]any, v any, path string) []string {
	arr, ok := v.([]any)
	if !ok {
		return []string{path + ": expected array"}
	}
	var out []string
	for i, val := range arr {
		if p, ok := s["items"].(map[string]any); ok {
			out = append(out, secondSchemaErrors(p, val, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return out
}
func secondPrimitive(typ string, v any) bool {
	switch typ {
	case "null":
		return v == nil
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		n, ok := v.(float64)
		return ok && n == float64(int64(n))
	}
	return true
}
