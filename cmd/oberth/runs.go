package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func publicText(value any) string { text, _ := value.(string); return text }
func renderRuns(out io.Writer, runs []any, page map[string]any, detail bool) error {
	if len(runs) == 0 {
		return writeWrapped(out, "no runs")
	}
	rows := [][]string{{"ID", "STATUS", "REF", "REASON"}}
	for _, row := range runs {
		run, ok := row.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid run row")
		}
		id := publicText(run["id"])
		ref := strings.TrimPrefix(publicText(run["ref"]), "refs/heads/")
		rows = append(rows, []string{id, publicText(run["status"]), ref, publicText(run["display_reason"])})

	}
	if err := renderTable(out, rows); err != nil {
		return err
	}
	if detail {
		for _, row := range runs {
			if err := renderRun(out, row.(map[string]any), true); err != nil {
				return err
			}
		}
	}
	if cursor := publicText(page["next_cursor"]); cursor != "" {
		return writeWrapped(out, "Next page: oberth run list --before "+cursor)
	}
	return nil
}
func renderRun(out io.Writer, run map[string]any, detail bool) error {
	if err := writeWrapped(out, "Run "+publicText(run["id"])+"  "+publicText(run["status"])); err != nil {
		return err
	}
	if err := renderRunFacts(out, run, detail); err != nil {
		return err
	}
	if detail {
		if err := renderFacts(out, "detail", run["detail"], true); err != nil {
			return err
		}
	}
	if err := renderFailure(out, run, detail); err != nil {
		return err
	}
	steps, _ := run["steps"].([]any)
	if len(steps) == 0 {
		return nil
	}
	if err := renderSteps(out, steps, detail); err != nil {
		return err
	}

	return writeWrapped(out, "Logs: oberth log "+publicText(run["id"]))
}

// renderRunFacts shows where the run came from beside what it ran; assistance
// appears only when the push asked for it.
func renderRunFacts(out io.Writer, run map[string]any, detail bool) error {
	for _, key := range []string{"repo", "ref", "commit_sha", "trigger", "schedule_entry", "rerun_of", "actor", "assist_requested", "display_reason", "next_step_offset"} {
		if key == "assist_requested" && run[key] != true {
			continue
		}
		if err := renderFacts(out, key, run[key], detail); err != nil {
			return err
		}
	}
	return nil
}

func renderFailure(out io.Writer, run map[string]any, detail bool) error {
	if err := renderFacts(out, "failure", run["failure"], detail); err != nil {
		return err
	}
	if failure, ok := run["failure"].(map[string]any); ok {
		if name := publicText(failure["failed_step_name"]); name != "" {
			if err := writeWrapped(out, "Failed step "+name+": "+publicText(run["display_reason"])); err != nil {
				return err
			}
		}
		if step := publicText(failure["failed_step"]); step != "" {
			return writeWrapped(out, "Failed-step logs: oberth log "+commandWord(publicText(run["id"]))+" "+commandWord(step))
		}
	}
	return nil
}

func commandWord(value string) string {
	if value != "" && strings.Trim(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._/") == "" {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func renderSteps(out io.Writer, steps []any, detail bool) error {
	rows := [][]string{{"STEP", "STATUS", "REASON"}}
	for _, value := range steps {
		step, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid step row")
		}
		name := stepName(step)
		number, _ := step["parallel_group"].(json.Number)
		if group, _ := number.Int64(); group > 0 {
			name = "│ " + name
		}
		rows = append(rows, []string{name, publicText(step["status"]), publicText(step["display_reason"])})
	}
	if err := renderTable(out, rows); err != nil {
		return err
	}
	if detail {
		for _, value := range steps {
			if err := renderFacts(out, "Step "+stepName(value.(map[string]any))+" detail", value.(map[string]any)["detail"], true); err != nil {
				return err
			}
		}
	}
	return nil
}

func renderWait(out io.Writer, value map[string]any, detail bool) error {
	if timedOut, _ := value["timed_out"].(bool); timedOut {
		if err := writeWrapped(out, "Wait timed out."); err != nil {
			return err
		}
	}
	if run, ok := value["run"].(map[string]any); ok {
		return renderRun(out, run, detail)
	}
	return writeWrapped(out, "No run matched.")
}
func renderPromotion(out io.Writer, value map[string]any, detail bool) error {
	if err := writeWrapped(out, "Promotion "+publicText(value["id"])+"  "+publicText(value["state"])); err != nil {
		return err
	}
	for _, key := range []string{"tested_run_id", "commit_sha", "run_id"} {
		if err := renderFacts(out, key, value[key], detail); err != nil {
			return err
		}
	}
	if id := publicText(value["run_id"]); id != "" {
		return writeWrapped(out, "Next: oberth run wait "+id)
	}
	return nil
}

func stepName(step map[string]any) string {
	if name := publicText(step["name"]); name != "" {
		return name
	}
	return publicText(step["plan_path"])
}
