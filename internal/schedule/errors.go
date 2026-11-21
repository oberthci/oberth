package schedule

import (
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
)

type diagnostic int

const (
	invalidLimits diagnostic = iota
	documentSize
	documentSyntax
	entryCount
	emptyList
	entryName
	duplicateName
	branchName
	unknownZone
	concurrencyPolicy
	calendarOccurrence
	cronSyntax
	unknownField
	fileProblems
)

func reasons() []string {
	return []string{
		"schedule entry limit is invalid",
		"schedule file is too large",
		"schedule file is not valid YAML",
		"too many schedules",
		"schedule list is empty",
		"schedule name is invalid",
		"schedule name is repeated",
		"schedule branch is invalid",
		"schedule time zone is invalid",
		"schedule concurrency policy is invalid",
		"schedule has no future occurrence",
		"schedule cron expression is invalid",
		"schedule field is unknown",
		"%d problems in .oberth/schedule.yaml",
	}
}

const remedy = "Fix the listed values in .oberth/schedule.yaml and push to the default branch."

func invalid(id diagnostic, detail string) *publicerr.Error {
	failure := publicerr.New(publicerr.InvalidInput, reasons()[id], remedy, nil)
	failure.Detail = detail
	return failure
}
func (id diagnostic) fieldError(label, field, value, advice string) *publicerr.Error {
	return invalid(id, fmt.Sprintf("%s: field %q has value %q: %s.", label, field, value, advice))
}

func problems(issues []*publicerr.Error) *publicerr.Error {
	if len(issues) == 1 {
		return issues[0]
	}
	failure := publicerr.New(publicerr.InvalidInput, fmt.Sprintf(reasons()[fileProblems], len(issues)), remedy, nil)
	details := make([]string, 0, len(issues))
	for _, issue := range issues {
		details = append(details, issue.Detail)
	}
	failure.Detail = strings.Join(details, "\n")
	return failure
}
