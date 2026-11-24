package main

import (
	"fmt"
	"io"
)

func renderLocks(out io.Writer, value map[string]any, detail bool) error {
	locks := listValue(value["locks"])
	if len(locks) == 0 {
		return writeWrapped(out, "no locks")
	}
	for _, item := range locks {
		if err := renderLock(out, item.(map[string]any), detail); err != nil {
			return err
		}
	}
	return nil
}

func renderLock(out io.Writer, lock map[string]any, detail bool) error {
	if _, err := fmt.Fprintf(out, "%s  %s held\n", publicText(lock["lock"]), publicText(lock["usage"])); err != nil {
		return err
	}
	rows := [][]string{{"HOLDER", "PERMITS", "STEPS"}}
	for _, item := range listValue(lock["holders"]) {
		holder := item.(map[string]any)
		rows = append(rows, []string{publicText(holder["run_id"]), fmt.Sprint(holder["permits"]), publicText(holder["steps"])})
	}
	if len(rows) > 1 {
		if err := renderTable(out, rows); err != nil {
			return err
		}
	}
	for _, field := range []string{"waiters", "stale", "remedy", "command"} {
		if lock[field] == nil || len(listValue(lock[field])) == 0 && (field == "waiters" || field == "stale") {
			continue
		}
		if err := renderFacts(out, field, lock[field], detail); err != nil {
			return err
		}
	}
	return nil
}

func listValue(value any) []any { rows, _ := value.([]any); return rows }
