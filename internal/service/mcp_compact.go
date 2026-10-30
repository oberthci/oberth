package service

import (
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// Full reads remain the default. BodyLimit counts retained source bytes, with
// the truncation marker added outside that budget. Occurrence history lives in
// Body; Occurrences itself is a numeric count and remains exact.
type issueReadOptions struct {
	Fields    []string `json:"fields"`
	BodyLimit *int     `json:"body_limit"`
}

func (options issueReadOptions) validate() error {
	if options.BodyLimit != nil && *options.BodyLimit < 0 {
		return fmt.Errorf("%w: body_limit must be nonnegative", ErrInvalidInput)
	}
	if options.Fields != nil && len(options.Fields) == 0 {
		return fmt.Errorf("%w: fields must be a nonempty list of issue field names", ErrInvalidInput)
	}
	seen := make(map[string]bool, len(options.Fields))
	for _, field := range options.Fields {
		if !slices.Contains(api.IssueReadFields, field) || seen[field] {
			return fmt.Errorf("%w: fields must contain unique supported issue field names", ErrInvalidInput)
		}
		seen[field] = true
	}
	return nil
}

func (options issueReadOptions) project(issue model.Issue) any {
	wire := wireIssue(issue)
	if options.Fields != nil && !slices.Contains(options.Fields, "body") {
		wire.Body = ""
	} else if options.BodyLimit != nil {
		wire.Body = limitIssueBody(wire.Body, *options.BodyLimit)
	}
	if options.Fields == nil {
		return wire
	}
	values := issueFieldValues(wire)
	selected := make(map[string]any, len(options.Fields))
	for _, field := range options.Fields {
		// Selected zero values are explicit, even for fields normally omitted.
		selected[field] = values[field]
	}
	return selected
}

func limitIssueBody(body string, limit int) string {
	if len(body) <= limit {
		return body
	}
	end := limit
	// Do not split a UTF-8 code point. Count all withheld source bytes.
	for end > 0 && !utf8.RuneStart(body[end]) {
		end--
	}
	return fmt.Sprintf("%s\n[truncated %d bytes]", body[:end], len(body)-end)
}

func issueFieldValues(issue api.IssueResponse) map[string]any {
	return map[string]any{
		"id": issue.ID, "lock": issue.Lock, "upstream": issue.Upstream,
		"kind": issue.Kind, "title": issue.Title, "body": issue.Body,
		"state": issue.State, "repo_id": issue.RepoID, "branch": issue.Branch,
		"occurrences": issue.Occurrences, "ci_origin": issue.CIOrigin,
		"ci_work_id": issue.CIWorkID, "created_at": issue.CreatedAt,
		"updated_at": issue.UpdatedAt,
	}
}

func issueReadBytes(value any) int {
	var fields map[string]any
	switch typed := value.(type) {
	case api.IssueResponse:
		fields = issueFieldValues(typed)
	case map[string]any:
		fields = typed
	}
	size := 0
	for _, field := range fields {
		if text, ok := field.(string); ok {
			size += len(text)
		}
	}
	return size
}

func wireIssueMutation(issue model.Issue, appendedBytes int) api.IssueMutationResponse {
	return api.IssueMutationResponse{
		ID: issue.ID, State: string(issue.State),
		UpdatedAt:   issue.UpdatedAt.UTC().Format(time.RFC3339),
		Occurrences: issue.Occurrences, AppendedBytes: appendedBytes,
	}
}

// RunSummaryResponse deliberately excludes retained Timing, repository data,
// diagnostics and telemetry. Unknown step times remain null.
type RunSummaryResponse struct {
	ID         string           `json:"id"`
	Status     model.RunStatus  `json:"status"`
	Phase      string           `json:"phase"`
	FailedBurn string           `json:"failed_burn"`
	FailedStep string           `json:"failed_step"`
	Error      string           `json:"error"`
	Steps      []RunStepSummary `json:"steps"`
}

type RunStepSummary struct {
	Burn       string           `json:"burn"`
	Name       string           `json:"name"`
	Status     model.StepStatus `json:"status"`
	StartedAt  *time.Time       `json:"started_at"`
	FinishedAt *time.Time       `json:"finished_at"`
}

func summarizeRun(run model.Run, steps []model.StepResult) RunSummaryResponse {
	response := RunSummaryResponse{
		ID: run.ID, Status: run.Status, Phase: run.Phase,
		FailedBurn: run.FailedBurn, FailedStep: run.FailedStep, Error: run.Error,
		Steps: make([]RunStepSummary, len(steps)),
	}
	for i, step := range steps {
		response.Steps[i] = RunStepSummary{
			Burn: step.Burn, Name: step.Step, Status: step.Status,
			StartedAt: step.StartedAt, FinishedAt: step.FinishedAt,
		}
	}
	return response
}
