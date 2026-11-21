package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

func renderTable(out io.Writer, rows [][]string) error {
	var text strings.Builder
	table := tabwriter.NewWriter(&text, 0, 4, 2, ' ', 0)
	for _, row := range rows {
		if _, err := fmt.Fprintln(table, strings.Join(row, "\t")); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSuffix(text.String(), "\n"), "\n") {
		if _, err := fmt.Fprintln(out, strings.TrimRight(line, " ")); err != nil {
			return err
		}
	}
	return nil
}

func factLabel(path string) string {
	path = strings.ReplaceAll(path, "commit_sha", "commit")
	path = strings.ReplaceAll(path, "display_reason", "reason")
	path = strings.ReplaceAll(path, "_", " ")
	return strings.ReplaceAll(path, ".", " / ")
}

func renderLatestRun(out io.Writer, path string, run map[string]any) error {
	text := publicText(run["id"]) + "  " + publicText(run["status"])
	if reason := publicText(run["display_reason"]); reason != "" {
		text += "  " + reason
	}
	return writeWrapped(out, factLabel(path)+": "+text)
}
