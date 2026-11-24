package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/oberthci/oberth/internal/apicontract"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

func (c command) resultOptions(o commonOptions, refused bool) commonOptions {
	o.resultType = c.definition.ResultType
	o.detail = o.detail || c.definition.Name == "schedule_list" || refused && c.definition.Name == "access_revoke"
	return o
}

func (a application) render(data json.RawMessage, refused bool, o commonOptions) error {
	if o.json {
		_, err := fmt.Fprintln(a.out, string(data))
		return err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if refused {
		return a.renderRefusal(data, o)
	}
	declareEmptyLists(value, o.resultType)
	return renderFacts(a.out, "", value, o.detail)
}

func renderFacts(out io.Writer, path string, value any, detail bool) error {
	switch v := value.(type) {
	case map[string]any:
		return renderObject(out, path, v, detail)
	case []any:
		if len(v) == 0 {
			return writeWrapped(out, "no "+strings.ReplaceAll(path, "_", " "))
		}
		for i, child := range v {
			if err := renderFacts(out, path+"["+strconv.Itoa(i)+"]", child, detail); err != nil {
				return err
			}
		}
	default:
		if v == nil || v == "" {
			return nil
		}
		if path == "" {
			path = "result"
		}
		text := fmt.Sprint(v)
		for _, line := range strings.Split(text, "\n") {
			if _, err := fmt.Fprintln(out, factLine(factLabel(path), line)); err != nil {
				return err
			}
		}
	}
	return nil
}

func renderObject(out io.Writer, path string, value map[string]any, detail bool) error {
	if handled, err := renderView(out, value, detail); handled {
		return err
	}
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if key == "detail" && !detail {
			continue
		}
		label := key
		if path != "" {
			label = path + "." + key
		}
		if handled, err := renderRepositoryRuns(out, label, key, value[key]); handled || err != nil {
			if err != nil {
				return err
			}
			continue
		}
		if err := renderFacts(out, label, value[key], detail); err != nil {
			return err
		}
	}
	return nil
}

func factLine(path, value string) string {
	var out strings.Builder
	_ = writeWrapped(&out, path+": "+value)
	return strings.TrimSuffix(out.String(), "\n")
}

func renderView(out io.Writer, value map[string]any, detail bool) (bool, error) {
	if _, ok := value["locks"].([]any); ok {
		return true, renderLocks(out, value, detail)
	}
	if _, ok := value["settings"].([]any); ok {
		return true, renderSettings(out, value, detail)
	}
	if _, ok := value["tested_run_id"]; ok {
		return true, renderPromotion(out, value, detail)
	}
	if _, ok := value["run"]; ok && value["timed_out"] != nil {
		return true, renderWait(out, value, detail)
	}
	if runs, ok := value["runs"].([]any); ok {
		return true, renderRuns(out, runs, value, detail)
	}
	if _, ok := value["counts"].(map[string]any); ok && value["issues"] != nil {
		return true, renderIssues(out, value, detail)
	}
	if value["id"] != nil && value["status"] != nil && value["ref"] != nil {
		return true, renderRun(out, value, detail)
	}
	return false, nil
}

// JSON null and [] both denote an empty list for a declared slice field.
func declareEmptyLists(value any, t reflect.Type) {
	object, ok := value.(map[string]any)
	if !ok || t == nil {
		return
	}
	for _, f := range apicontract.Arguments(t) {
		if f.Field.Type.Kind() == reflect.Slice && object[f.Name] == nil {
			object[f.Name] = []any{}
		}
	}
}

func (a application) renderRefusal(data json.RawMessage, o commonOptions) error {
	var refusal refusalMessage
	if err := json.Unmarshal(data, &refusal); err != nil {
		return err
	}
	if refusal.Reason == "" || refusal.Remedy == "" {
		return fmt.Errorf("invalid refusal")
	}
	return a.writeRefusal(refusal, o.detail)
}

type refusalMessage struct {
	Context                []string
	Reason, Remedy, Detail string
}

func (a application) writeRefusal(refusal refusalMessage, detail bool) error {
	lines := append([]string{refusal.Reason, refusal.Remedy}, refusal.Context...)
	if refusal.Detail != "" {
		text := "More detail is available with --detail."
		if detail {
			text = refusal.Detail
		}
		lines = append(lines, text)
	}
	for _, line := range lines {
		if err := writeWrapped(a.diagnostic, safeDisplay(line)); err != nil {
			return err
		}
	}
	return nil
}
