package strictjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"

	"github.com/oberthci/oberth/internal/publicerr"
)

// DecodeStrictObject decodes one bounded, non-null JSON object, rejecting unknown
// fields and trailing data. Standard JSON rules apply: null equals omission for
// optional pointers, field matching ignores case, and the last duplicate wins.
// On refusal dst is unchanged; on success it is replaced, clearing omissions.
// Callers supply {} when tool arguments are omitted and bound external reads.
func DecodeStrictObject(data []byte, dst any, maxBytes int) error {
	if maxBytes <= 0 {
		return invalid("invalid JSON size limit", "configure a positive request size limit", nil)
	}
	if len(data) > maxBytes {
		return invalid(fmt.Sprintf("JSON request exceeds the %d-byte limit", maxBytes), "reduce the request size", nil)
	}
	target := reflect.ValueOf(dst)
	if target.Kind() != reflect.Pointer || target.IsNil() || target.Elem().Kind() != reflect.Struct {
		return invalid("invalid JSON destination", "supply a non-nil struct pointer", nil)
	}
	body := bytes.TrimSpace(data)
	if len(body) == 0 || body[0] != '{' {
		return invalid("expected a JSON object", "supply a non-null object, or {} for no arguments", nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	fresh := reflect.New(target.Elem().Type())
	if err := decoder.Decode(fresh.Interface()); err != nil {
		return decodeError(err, body, fresh.Elem().Type())
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return invalid("unexpected data after the JSON object", "supply exactly one JSON object", err)
	}
	target.Elem().Set(fresh.Elem())
	return nil
}
func invalid(reason, remedy string, cause error) error {
	return publicerr.New(publicerr.InvalidInput, reason, remedy, cause)
}

func invalidDetail(reason, remedy, detail string, cause error) error {
	e := publicerr.New(publicerr.InvalidInput, reason, remedy, cause)
	e.Detail = detail
	return e
}
