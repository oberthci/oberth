// Package recordjson maps Go record field names to the snake_case names the
// HTTP and MCP API sends, in both directions.
package recordjson

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"unicode"
)

// Field is one exported field of a record: its index, the name encoding/json
// writes for it (the tag name, or the Go name when untagged), and that
// name's snake_case wire spelling.
type Field struct {
	Index       int
	Name, Alias string
	Embedded    bool
}

var plans sync.Map // reflect.Type -> []Field, immutable after publication.

// Plan lists typ's encoded fields; typ must be a struct type.
func Plan(typ reflect.Type) []Field {
	if plan, ok := plans.Load(typ); ok {
		return plan.([]Field)
	}
	fields := make([]Field, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" && !embeddedStruct(field) {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		embedded := field.Anonymous && name == ""
		if name == "" {
			name = field.Name
		}
		fields = append(fields, Field{Index: i, Name: name, Alias: SnakeCase(name), Embedded: embedded})
	}
	plan, _ := plans.LoadOrStore(typ, fields)
	return plan.([]Field)
}

// embeddedStruct reports an embedded struct whose exported fields
// encoding/json promotes even when the embedded type itself is unexported.
func embeddedStruct(field reflect.StructField) bool {
	typ := field.Type
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return field.Anonymous && typ.Kind() == reflect.Struct
}

// SnakeCase spells a Go field name the way the API sends it: RepoID ->
// repo_id, SHA -> sha, QueueSequence -> queue_sequence.
func SnakeCase(name string) string {
	runes := []rune(name)
	var out strings.Builder
	for i, r := range runes {
		if unicode.IsUpper(r) {
			if i > 0 && runes[i-1] != '_' && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]) || (unicode.IsUpper(runes[i-1]) && i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
				out.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		out.WriteRune(r)
	}
	return out.String()
}

var jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// Unmarshal decodes an API response (snake_case record names) into a
// Go record whose fields carry no json tags (model.Run and friends): each
// snake_case wire name is mapped back to its field name first. Any other
// spelling in the response (ID, RepoID, activeDeadlineSeconds) is ignored.
func Unmarshal(data []byte, into any) error {
	target := reflect.TypeOf(into)
	if target == nil || target.Kind() != reflect.Pointer {
		return json.Unmarshal(data, into)
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		// Report the decoder's own error for malformed input.
		return json.Unmarshal(data, into)
	}
	if !restoreRecordNames(document, target.Elem()) {
		return json.Unmarshal(data, into)
	}
	restored, err := json.Marshal(document)
	if err != nil {
		return err
	}
	return json.Unmarshal(restored, into)
}

func restoreRecordNames(document any, typ reflect.Type) bool {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Implements(jsonUnmarshalerType) || reflect.PointerTo(typ).Implements(jsonUnmarshalerType) {
		return false
	}
	changed := false
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := document.(map[string]any)
		if !ok {
			return false
		}
		for _, field := range Plan(typ) {
			fieldType := typ.Field(field.Index).Type
			if field.Embedded {
				changed = restoreRecordNames(object, fieldType) || changed
				continue
			}
			if field.Alias != field.Name {
				// Only the snake_case spelling is read; another spelling
				// in the document (ID, activeDeadlineSeconds) is dropped.
				if _, exists := object[field.Name]; exists {
					delete(object, field.Name)
					changed = true
				}
				if item, aliased := object[field.Alias]; aliased {
					object[field.Name] = item
					changed = true
				}
			}
			if item, exists := object[field.Name]; exists {
				changed = restoreRecordNames(item, fieldType) || changed
			}
		}
	case reflect.Slice, reflect.Array:
		rows, ok := document.([]any)
		if !ok {
			return false
		}
		for _, row := range rows {
			changed = restoreRecordNames(row, typ.Elem()) || changed
		}
	case reflect.Map:
		object, ok := document.(map[string]any)
		if !ok {
			return false
		}
		for _, item := range object {
			changed = restoreRecordNames(item, typ.Elem()) || changed
		}
	}
	return changed
}
