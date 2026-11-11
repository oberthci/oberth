package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"

	"github.com/oberthci/oberth/internal/recordjson"
)

// marshalRecords renames typed public records to their snake_case wire
// names after authorization/redaction (ID -> id, RepoID -> repo_id, Steps ->
// steps). Map keys are dictionaries (step names, labels, etc.), not Go field
// names.
func marshalRecords(payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&document); err != nil {
		return nil, err
	}
	changed := false
	addRecordAliases(document, reflect.ValueOf(payload), &changed)
	if !changed {
		return raw, nil
	}
	return json.Marshal(document)
}

func addRecordAliases(document any, value reflect.Value, changed *bool) {
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.Struct:
		object, ok := document.(map[string]any)
		if !ok {
			return
		}
		typ := value.Type()
		for _, field := range recordjson.Plan(typ) {
			if field.Embedded {
				addRecordAliases(object, value.Field(field.Index), changed)
				continue
			}
			item, exists := object[field.Name]
			if !exists {
				continue
			}
			addRecordAliases(item, value.Field(field.Index), changed)
			if field.Alias == field.Name {
				continue
			}
			// The snake_case name carries the value alone, also for a json
			// tag spelled in lowerCamel (activeDeadlineSeconds). A name whose
			// snake_case spelling is another field's key stays.
			if _, exists := object[field.Alias]; !exists {
				object[field.Alias] = item
				delete(object, field.Name)
				*changed = true
			}
		}
	case reflect.Slice, reflect.Array:
		rows, ok := document.([]any)
		if !ok {
			return
		}
		for i := 0; i < value.Len() && i < len(rows); i++ {
			addRecordAliases(rows[i], value.Index(i), changed)
		}
	case reflect.Map:
		object, ok := document.(map[string]any)
		if !ok || value.Type().Key().Kind() != reflect.String {
			return
		}
		iterator := value.MapRange()
		for iterator.Next() {
			key := iterator.Key().String()
			if item, exists := object[key]; exists {
				addRecordAliases(item, iterator.Value(), changed)
			}
		}
	}
}

// writeCollectionView wraps a collection in the object MCP returns: the rows
// under field and, for a full run page, the cursor of the next page.
func (server *Server) writeCollectionView(w http.ResponseWriter, r *http.Request, field string, value any, err error) {
	if err == nil {
		kind := reflect.Invalid
		if value != nil {
			kind = reflect.TypeOf(value).Kind()
		}
		if kind == reflect.Slice || kind == reflect.Array || value == nil {
			if value == nil || (kind == reflect.Slice && reflect.ValueOf(value).IsNil()) {
				value = []any{}
			}
			object := map[string]any{field: value}
			if cursor := w.Header().Get("X-Oberth-Next-Before"); cursor != "" {
				if !actorFrom(r.Context()).Scoped() {
					if n, e := strconv.ParseInt(cursor, 10, 64); e == nil {
						object["next_before"] = n
					} else {
						object["next_cursor"] = cursor
					}
				} else {
					object["next_cursor"] = cursor
				}
			}
			value = object
		}
	}
	server.writeView(w, value, err)
}
