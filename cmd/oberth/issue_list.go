package main

import (
	"encoding/json"
	"io"
	"strconv"
)

// renderIssues leads with the list's counts on one line, then the page's
// place in the filtered total, its issues and the commands that continue.
func renderIssues(out io.Writer, page map[string]any, detail bool) error {
	counts, _ := page["counts"].(map[string]any)
	count := func(key string) string { return pageNumber(counts[key]) + " " + key }
	summary := count("open") + " (" + count("todo") + ", " + count("active") + ", " + count("waiting") + ", " +
		count("review") + ", " + count("stale") + "), " + count("closed")
	if err := writeWrapped(out, summary); err != nil {
		return err
	}
	issues, _ := page["issues"].([]any)
	if len(issues) > 0 {
		offset, _ := strconv.Atoi(pageNumber(page["offset"]))
		position := "Showing " + strconv.Itoa(offset+1) + "-" + strconv.Itoa(offset+len(issues)) + " of " + pageNumber(page["total"]) + "."
		if err := writeWrapped(out, position); err != nil {
			return err
		}
	}
	if err := renderFacts(out, "issues", issues, detail); err != nil {
		return err
	}
	if cursor := pageNumber(page["next_before"]); cursor != "" {
		if err := writeWrapped(out, "Older page: oberth issue list --before "+cursor); err != nil {
			return err
		}
	}
	if cursor := pageNumber(page["next_after"]); cursor != "" {
		return writeWrapped(out, "Newer page: oberth issue list --after "+cursor)
	}
	return nil
}

func pageNumber(value any) string {
	number, _ := value.(json.Number)
	return number.String()
}
