package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/store"
)

func toolArgumentNames(value any) []string {
	typ := reflect.TypeOf(value)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	var names []string
	for _, field := range reflect.VisibleFields(typ) {
		if field.PkgPath != "" || field.Anonymous {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}

// Only repeat selectors the caller supplied; never expose a hidden object's data.
func toolSelectorError(raw json.RawMessage, err error) error {
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil {
		return err
	}
	for _, key := range []string{"repo", "id", "sha", "ref", "run", "name"} {
		value, ok := args[key]
		if !ok {
			continue
		}
		var selector string
		if json.Unmarshal(value, &selector) != nil {
			var number json.Number
			if json.Unmarshal(value, &number) != nil {
				continue
			}
			selector = number.String()
		}
		if selector == "" {
			continue
		}
		if strings.Contains(err.Error(), selector) {
			return err
		}
		return fmt.Errorf("%w (%s %q)", err, key, selector)
	}
	return err
}
