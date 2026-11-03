package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// marshalCompatible adds preferred wire names to typed public records after
// authorization/redaction. Existing names and envelopes are retained. Map keys
// are dictionaries (step names, labels, etc.), not Go record field names.
func marshalCompatible(payload any) ([]byte, error) {
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

type recordAliasField struct {
	index       int
	name, alias string
	embedded    bool
}

var recordAliasPlans sync.Map // reflect.Type -> []recordAliasField, immutable after publication.

func recordAliasPlan(typ reflect.Type) []recordAliasField {
	if plan, ok := recordAliasPlans.Load(typ); ok {
		return plan.([]recordAliasField)
	}
	fields := make([]recordAliasField, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
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
		fields = append(fields, recordAliasField{index: i, name: name, alias: snakeCase(name), embedded: embedded})
	}
	plan, _ := recordAliasPlans.LoadOrStore(typ, fields)
	return plan.([]recordAliasField)
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
		for _, field := range recordAliasPlan(typ) {
			if field.embedded {
				addRecordAliases(object, value.Field(field.index), changed)
				continue
			}
			item, exists := object[field.name]
			if !exists {
				continue
			}
			addRecordAliases(item, value.Field(field.index), changed)
			if _, exists := object[field.alias]; !exists {
				object[field.alias] = item
				*changed = true
			}
		}
		// Only primary run records get the identity alias. A promotion's run_id
		// remains a related run and must never overwrite its promotion id.
		switch typ.Name() {
		case "Run", "RunSummaryResponse", "RunCancelResponse", "RunRerunResponse", "LogResponse", "ArtifactListResponse", "ArtifactResponse", "KVMPreflightResponse", "LiveLogResponse", "StatusResponse":
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

func snakeCase(name string) string {
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
	case "run_get", "run_logs", "run_retry", "run_cancel", "run_rerun", "run_purge", "run_kvm_preflight", "artifacts", "artifact_get", "wait":
		return "id"
	case "run_resources":
		return "run"
	}
	return ""
}
