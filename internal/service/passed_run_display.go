package service

import (
	"fmt"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

func passedRunDisplayReason(run model.Run) string {
	issues := make(map[int64]bool, len(run.IssueTrailers))
	for _, trailer := range run.IssueTrailers {
		if trailer.IssueID > 0 && trailer.SHA == run.SHA {
			issues[trailer.IssueID] = issues[trailer.IssueID] || trailer.Close
		}
	}
	ids := make([]int64, 0, len(issues))
	for id := range issues {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var closes, refs []string
	for _, id := range ids {
		ref := fmt.Sprintf("#%d", id)
		if issues[id] {
			closes = append(closes, ref)
		} else {
			refs = append(refs, ref)
		}
	}
	var parts []string
	if len(closes) > 0 {
		parts = append(parts, "closes "+strings.Join(closes, ", "))
	}
	if len(refs) > 0 {
		parts = append(parts, "refs "+strings.Join(refs, ", "))
	}
	if run.Trigger == "schedule" && run.ScheduleEntry != "" {
		parts = append(parts, "schedule "+run.ScheduleEntry)
	}
	return strings.Join(parts, " · ")
}

// assistDisplayReason is the reason-line fragment of an assisted run. The
// status stays passed or failed; the line says why it is not evidence and
// was not published. A requested-but-unused hold says nothing.
func assistDisplayReason(run model.Run) string {
	if run.Assist == nil {
		return ""
	}
	if reason := run.Assist.Reason(); reason != "" {
		return reason
	}
	if run.Assisted {
		return "assisted; not evidence"
	}
	return ""
}
