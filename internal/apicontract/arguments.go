package apicontract

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Argument describes one declared field. Position zero means flags only;
// Positional means the command line takes it only by its position.
type Argument struct {
	Name                            string
	Field                           reflect.StructField
	Required, Sensitive, Positional bool
	Position                        int
}

// Arguments flattens embedded declarations without assigning implicit positions.
func Arguments(t reflect.Type) []Argument {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var fields []Argument
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			for _, child := range Arguments(f.Type) {
				child.Field.Index = append([]int{i}, child.Field.Index...)
				fields = append(fields, child)
			}
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		position, _ := strconv.Atoi(f.Tag.Get("position"))
		fields = append(fields, Argument{Name: name, Field: f, Required: f.Tag.Get("required") == "true", Sensitive: f.Tag.Get("sensitive") == "true", Positional: position > 0 && f.Tag.Get("flag") == "-", Position: position})
	}
	return fields
}

// TimeoutField is the single interpretation of a declaration's timeout tag.
func TimeoutField(t reflect.Type) (Argument, bool) {
	for _, f := range Arguments(t) {
		if f.Field.Tag.Get("timeout") == "true" {
			return f, true
		}
	}
	return Argument{}, false
}

// TimeoutSeconds reads the requested timeout or its declared default.
func TimeoutSeconds(d Definition, input json.RawMessage) (string, int) {
	f, ok := TimeoutField(d.InputType)
	if !ok {
		return "", 0
	}
	var args map[string]json.RawMessage
	_ = json.Unmarshal(input, &args)
	var seconds int
	_ = json.Unmarshal(args[f.Name], &seconds)
	if seconds == 0 {
		seconds, _ = strconv.Atoi(f.Field.Tag.Get("default"))
	}
	return f.Name, seconds
}

// ApplyDefaults assigns only declared zero-value defaults to a decoded input.
func ApplyDefaults(v reflect.Value) {
	for _, f := range Arguments(v.Type()) {
		value := v.FieldByIndex(f.Field.Index)
		text := f.Field.Tag.Get("default")
		if text == "" || !value.IsZero() {
			continue
		}
		switch value.Kind() {
		case reflect.String:
			value.SetString(text)
		case reflect.Int, reflect.Int64:
			n, _ := strconv.ParseInt(text, 10, 64)
			value.SetInt(n)
		}
	}
}

// InputTimeout clamps the declared timeout and updates the decoded input.
func InputTimeout(v reflect.Value, maximum time.Duration) time.Duration {
	f, ok := TimeoutField(v.Type())
	if !ok {
		return 0
	}
	value := v.FieldByIndex(f.Field.Index)
	duration := min(time.Duration(value.Int())*time.Second, maximum)
	value.SetInt(int64(duration / time.Second))
	return duration
}
