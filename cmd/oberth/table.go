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

// renderRepositoryRuns prints a repository's latest run and its recent runs on
// one line each instead of as nested run views.
func renderRepositoryRuns(out io.Writer, path, key string, value any) (bool, error) {
	if run, ok := value.(map[string]any); key == "latest_run" && ok {
		return true, renderLatestRun(out, path, run)
	}
	if runs, ok := value.([]any); key == "recent_runs" && ok {
		text := "no runs"
		if len(runs) > 0 {
			text = recentLetters(runs) + " (oldest first)"
		}
		return true, writeWrapped(out, factLabel(path)+": "+text)
	}
	return false, nil
}

// recentLetters spells runs as the web page colours them: P passed, F failed,
// R queued or running, - cancelled or any other status.
func recentLetters(runs []any) string {
	letters := map[string]string{"passed": "P", "failed": "F", "queued": "R", "running": "R"}
	var text strings.Builder
	for _, run := range runs {
		object, _ := run.(map[string]any)
		status, _ := object["status"].(string)
		letter, ok := letters[status]
		if !ok {
			letter = "-"
		}
		text.WriteString(letter)
	}
	return text.String()
}
