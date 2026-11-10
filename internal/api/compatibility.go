package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"

	"github.com/oberthci/oberth/internal/recordjson"
)

// LegacyFieldsHeader opts one HTTP or MCP request into the pre-v0.20.3 record
// shape, where every Go-style field name (ID, RepoID, Steps) is sent next to
// its snake_case name. The server echoes it on responses that honour it.
const LegacyFieldsHeader = "X-Oberth-Legacy-Fields"

type legacyFieldsKey struct{}

// legacyFieldsRequested reports an explicit opt-in: the header, or the
// legacy_fields=1 query parameter on HTTP routes.
func legacyFieldsRequested(request *http.Request) bool {
	return request.Header.Get(LegacyFieldsHeader) == "1" || request.URL.Query().Get("legacy_fields") == "1"
}

func withLegacyFields(ctx context.Context) context.Context {
	return context.WithValue(ctx, legacyFieldsKey{}, true)
}

func legacyFields(ctx context.Context) bool {
	legacy, _ := ctx.Value(legacyFieldsKey{}).(bool)
	return legacy
}

// marshalCompatible renames typed public records to their snake_case wire
// names after authorization/redaction (ID -> id, RepoID -> repo_id, Steps ->
// steps). With legacy set, the Go-style names are kept next to the
// snake_case ones, the shape every release before v0.20.3 sent by default.
// Map keys are dictionaries (step names, labels, etc.), not Go field names.
func marshalCompatible(payload any, legacy bool) ([]byte, error) {
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
	addRecordAliases(document, reflect.ValueOf(payload), legacy, &changed)
	if !changed {
		return raw, nil
	}
	return json.Marshal(document)
}

func addRecordAliases(document any, value reflect.Value, legacy bool, changed *bool) {
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
				addRecordAliases(object, value.Field(field.Index), legacy, changed)
				continue
			}
			item, exists := object[field.Name]
			if !exists {
				continue
			}
			addRecordAliases(item, value.Field(field.Index), legacy, changed)
			if field.Alias == field.Name {
				continue
			}
			if _, exists := object[field.Alias]; !exists {
				object[field.Alias] = item
				*changed = true
				if !legacy {
					// The snake_case name now carries the value alone. A
					// name whose alias is another field's key stays.
					delete(object, field.Name)
				}
			}
		}
		// Only primary run records get the identity alias. A promotion's run_id
		// remains a related run and must never overwrite its promotion id.
		switch typ.Name() {
		case "Run", "RunSummaryResponse", "RunCancelResponse", "RunRerunResponse", "LogResponse", "ArtifactListResponse", "ArtifactResponse", "LiveLogResponse", "StatusResponse":
			if id, ok := object["id"]; ok {
				if _, exists := object["run_id"]; !exists {
					object["run_id"] = id
					*changed = true
				}
			} else if id, ok := object["run_id"]; ok {
				object["id"] = id
				*changed = true
			}
		case "RunDetailResponse":
			if run, ok := object["run"].(map[string]any); ok {
				if id, exists := run["id"]; exists {
					object["id"] = id
					*changed = true
					object["run_id"] = id
					*changed = true
				}
			}
		}
	case reflect.Slice, reflect.Array:
		rows, ok := document.([]any)
		if !ok {
			return
		}
		for i := 0; i < value.Len() && i < len(rows); i++ {
			addRecordAliases(rows[i], value.Index(i), legacy, changed)
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
				addRecordAliases(item, iterator.Value(), legacy, changed)
			}
		}
	}
}

// Legacy collection arrays are the default throughout v0.17.x. Callers can
// explicitly opt into the matching MCP object envelope without changing old
// clients. The authoritative cursor is the same one supplied in HTTP headers.
func (server *Server) writeCollectionView(w http.ResponseWriter, r *http.Request, field string, value any, err error) {
	if err == nil && r.URL.Query().Get("envelope") == "1" {
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

// RunIDArgument identifies existing run-selector fields whose additive alias is
// run_id. Kept in the wire package so schema advertisement and dispatch agree.
func RunIDArgument(tool string) string {
	switch tool {
	case "run_get", "run_logs", "run_retry", "run_cancel", "run_rerun", "run_hold_list", "run_hold_exec", "run_hold_patch", "run_continue", "run_hold_abandon", "run_purge", "artifacts", "artifact_get", "wait", "run_wait":
		return "id"
	case "run_resources":
		return "run"
	}
	return ""
}
