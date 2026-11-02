package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// collectIssuePages keeps the source transport's envelope and follows its
// cursor. Explicit CLI cursor/limit requests bypass this all-issues view.
func collectIssuePages(ctx context.Context, load func(context.Context, int64) (json.RawMessage, error)) (json.RawMessage, error) {
	var first map[string]json.RawMessage
	var all []json.RawMessage
	var before int64
	var issuesKey, cursorKey string
	for page := 0; page < 1000; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := load(ctx, before)
		if err != nil {
			return nil, err
		}
		var object map[string]json.RawMessage
		if err = json.Unmarshal(raw, &object); err != nil {
			return nil, err
		}
		if first == nil {
			first = object
			issuesKey, cursorKey = "issues", "next_before"
			if _, ok := object["Issues"]; ok {
				issuesKey, cursorKey = "Issues", "NextBefore"
			}
		}
		if _, ok := object[issuesKey]; !ok {
			// JSON output also supports older or extended server envelopes.
			if page == 0 {
				return raw, nil
			}
			return nil, fmt.Errorf("issue page is missing its rows")
		}
		var rows []json.RawMessage
		if err = json.Unmarshal(object[issuesKey], &rows); err != nil {
			return nil, fmt.Errorf("decode issue page: %w", err)
		}
		var next int64
		if len(object[cursorKey]) > 0 {
			if err = json.Unmarshal(object[cursorKey], &next); err != nil {
				return nil, err
			}
		}
		// A pre-paging server may repeat the same page. Never duplicate its rows.
		if before > 0 && next >= before {
			break
		}
		all = append(all, rows...)
		if next == 0 || len(rows) == 0 {
			break
		}
		if page == 999 {
			return nil, fmt.Errorf("issue pagination exceeded 1000 pages; use --before and --limit")
		}
		before = next
	}
	if all == nil {
		all = []json.RawMessage{}
	}
	encoded, err := json.Marshal(all)
	if err != nil {
		return nil, err
	}
	first[issuesKey] = encoded
	first[cursorKey] = json.RawMessage("0")
	// Preserve equality of additive aliases when combining HTTP pages.
	for _, key := range []string{"Issues", "issues"} {
		if _, exists := first[key]; exists {
			first[key] = encoded
		}
	}
	for _, key := range []string{"NextBefore", "next_before"} {
		if _, exists := first[key]; exists {
			first[key] = json.RawMessage("0")
		}
	}
	return json.Marshal(first)
}
