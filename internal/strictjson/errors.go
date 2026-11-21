package strictjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
)

func decodeError(err error, data []byte, typ reflect.Type) error {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return invalid("JSON object ended before it was complete", "complete the JSON object and close its strings and containers", err)
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return invalidDetail("JSON syntax is invalid.", "Correct the JSON syntax.", fmt.Sprintf("Byte %d: %s", syntax.Offset, syntax.Error()), err)
	}
	site := failedLocation(data, typ, "", err)
	var mismatch *json.UnmarshalTypeError
	if errors.As(site.cause, &mismatch) {
		reason := fmt.Sprintf("field %q: expected %s, got %s", site.path, expected(mismatch.Type), arrived(mismatch.Value))
		return invalidDetail("A JSON field has the wrong type or range.", "Use the expected field type and range.", reason, err)
	}
	var timestamp *time.ParseError
	if errors.As(site.cause, &timestamp) {
		return invalidDetail("A JSON field has an invalid timestamp.", "Use an RFC 3339 timestamp, for example 2026-01-02T03:04:05Z.", fmt.Sprintf("Field %q: expected an RFC 3339 timestamp", site.path), err)
	}
	if strings.HasPrefix(site.cause.Error(), "json: unknown field ") {
		accepted := "accepted fields: " + strings.Join(site.accepted, ", ")
		if len(site.accepted) == 0 {
			accepted = "this object accepts no fields; remove the unknown field"
		}
		return invalidDetail("A JSON field is unknown.", "Remove the unknown field or use a name listed in the details.", fmt.Sprintf("Unknown field %q; %s", site.path, accepted), err)
	}
	return invalidDetail("A JSON field could not be decoded.", "Check the value against its declared field format.", fmt.Sprintf("Field %q could not be decoded", site.path), err)
}

func expected(typ reflect.Type) string {
	switch typ.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Array, reflect.Slice:
		return "an array"
	default:
		return "an object"
	}
}

func arrived(value string) string {
	kind, content, _ := strings.Cut(value, " ")
	if kind == "number" && content != "" && !json.Valid([]byte(content)) {
		return "a non-numeric object key"
	}
	return kind
}
