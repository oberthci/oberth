package main

import (
	"io"
	"maps"
)

func renderSettings(out io.Writer, status map[string]any, detail bool) error {
	metadata := maps.Clone(status)
	delete(metadata, "settings")
	if err := renderObject(out, "", metadata, detail); err != nil {
		return err
	}
	rows := [][]string{{"SETTING", "SOURCE", "VALUE"}}
	for _, value := range status["settings"].([]any) {
		setting := value.(map[string]any)
		rows = append(rows, []string{publicText(setting["name"]), publicText(setting["source"]), publicText(setting["value"])})
	}
	return renderTable(out, rows)
}
