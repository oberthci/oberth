package api

import (
	"errors"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/strictjson"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

func decode(raw []byte, dst any, limit int) error {
	if !utf8.Valid(raw) {
		return refusal("input")
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	return strictjson.DecodeStrictObject(raw, dst, limit)
}
func validateInput(input any) error { return requiredFields(reflect.ValueOf(input)) }

// Only wire shape, bounded sizes and explicitly tagged repository syntax belong here.
// Selector combinations, authored paths and audit target resolution belong to C2.
func requiredFields(v reflect.Value) error {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		return requiredFields(v.Elem())
	}
	if v.Kind() == reflect.Slice {
		var errs []error
		for i := 0; i < v.Len(); i++ {
			errs = append(errs, requiredFields(v.Index(i)))
		}
		return errors.Join(errs...)
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	var errs []error
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		value := v.Field(i)
		key := strings.Split(f.Tag.Get("json"), ",")[0]
		if f.Tag.Get("required") == "true" && value.IsZero() {
			errs = append(errs, argumentError(key))
			continue
		}
		if err := fieldBounds(f, value); err != nil {
			errs = append(errs, argumentError(key))
		}
		errs = append(errs, requiredFields(value))
	}
	return errors.Join(errs...)
}
func fieldBounds(f reflect.StructField, v reflect.Value) error {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if (v.Kind() == reflect.Slice || v.Kind() == reflect.Map) && v.IsNil() && f.Tag.Get("required") != "true" {
		return nil
	}
	if err := repositoryField(f, v); err != nil {
		return err
	}
	for _, tag := range []string{"min", "max", "minItems", "maxItems", "maxBytes"} {
		if exceedsBound(f, v, tag) {
			return refusal("input")
		}
	}

	return nil
}
func argumentError(field string) error {
	e := publicerr.New(publicerr.InvalidInput, "Invalid request argument: "+field+".", "Use the declared type and limits for this field.", nil)
	e.Detail = "Check field: " + field
	return e
}

func logDefaults(v reflect.Value) {
	for _, name := range []string{"Pattern", "Context", "Offset", "ByteOffset", "Limit", "Tail"} {
		if !v.FieldByName(name).IsNil() {
			return
		}
	}
	tail, limit := true, 200
	v.FieldByName("Tail").Set(reflect.ValueOf(&tail))
	v.FieldByName("Limit").Set(reflect.ValueOf(&limit))
}

func exceedsBound(f reflect.StructField, v reflect.Value, tag string) bool {
	text := f.Tag.Get(tag)
	if text == "" {
		return false
	}
	bound, _ := strconv.ParseInt(text, 10, 64)
	n, ok := boundValue(v)
	if !ok {
		return false
	}
	if v.Kind() == reflect.Int && n == 0 && f.Tag.Get("required") != "true" {
		return false
	}
	if tag == "min" || tag == "minItems" {
		return n < bound
	}
	return n > bound
}
func boundValue(v reflect.Value) (int64, bool) {
	switch v.Kind() {
	case reflect.Int, reflect.Int64:
		return v.Int(), true
	case reflect.Slice, reflect.String, reflect.Map:
		return int64(v.Len()), true
	}
	return 0, false
}

func repositoryField(f reflect.StructField, v reflect.Value) error {
	if v.Kind() == reflect.String && f.Tag.Get("format") == "repo" && v.String() != "" {
		return reponame.ValidateSelector(v.String())
	}
	if f.Tag.Get("format") == "repos" {
		for i := 0; i < v.Len(); i++ {
			if err := reponame.ValidateSelector(v.Index(i).String()); err != nil {
				return err
			}
		}
	}
	return nil
}
