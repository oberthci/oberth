package stateport

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// IssueImport binds validated neutral records to the exact file digest.
// Record order is irrelevant to replay; each history slice retains its order.
type IssueImport struct {
	Records    []model.IssueImportRecord
	FileSHA256 string
	DryRun     bool
}

type IssueImportResult struct {
	Count           int    `json:"count"`
	HistoryEntries  int    `json:"history_entries"`
	SHA256          string `json:"sha256"`
	DryRun          bool   `json:"dry_run"`
	AlreadyImported bool   `json:"already_imported"`
	Outcome         string `json:"outcome,omitempty"`
}

func (q IssueImport) Validate() error {
	if len(q.Records) == 0 || !digest(q.FileSHA256) {
		return ImportProblem(ImportFile, 1, 0, nil)
	}
	ids := map[int64]bool{}
	for i, v := range q.Records {
		key := importedIssueProblem(v)
		if ids[v.ID] {
			key = ImportDuplicate
		}
		if key != noImportProblem {
			return ImportProblem(key, q.Line(i), v.ID, nil)
		}
		ids[v.ID] = true
	}
	return nil
}

func importedIssueProblem(v model.IssueImportRecord) ImportProblemKey {
	if v.ID <= 0 || v.ID == math.MaxInt64 {
		return ImportID
	}
	if strings.TrimSpace(v.Title) == "" {
		return ImportTitle
	}
	if key := importedMetadataProblem(v); key != noImportProblem {
		return key
	}
	if key := importedTimeProblem(v); key != noImportProblem {
		return key
	}
	if v.Repo != "" && !strings.HasPrefix(v.Repo, v.Upstream+"/") {
		return ImportScope
	}
	for _, h := range v.History {
		if !validImportedHistory(h) {
			return ImportHistory
		}
	}
	return noImportProblem
}

func importedMetadataProblem(v model.IssueImportRecord) ImportProblemKey {
	switch {
	case !v.State.Valid():
		return ImportState
	case v.Kind != model.IssueManual:
		return ImportKind
	case !v.WorkStatus.Valid():
		return ImportWorkStatus
	case v.Occurrences <= 0:
		return ImportOccurrences
	}
	return noImportProblem
}

func importedTimeProblem(v model.IssueImportRecord) ImportProblemKey {
	if v.CreatedAt.IsZero() {
		return ImportCreatedAt
	}
	if v.UpdatedAt.IsZero() {
		return ImportUpdatedAt
	}
	if (v.State == model.IssueClosed) != (v.ClosedAt != nil) {
		return ImportClosedAt
	}
	if v.ClosedAt != nil && v.ClosedAt.IsZero() {
		return ImportClosedAt
	}
	return noImportProblem
}

func validImportedHistory(h model.IssueHistory) bool {
	if h.At.IsZero() {
		return false
	}
	switch h.Kind {
	case "state":
		return h.State.Valid()
	case "comment", "append":
		return h.State == ""
	default:
		return false
	}
}

func (q IssueImport) Source() (model.IssueImportSource, error) {
	records := slices.Clone(q.Records)
	slices.SortFunc(records, func(a, b model.IssueImportRecord) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	raw, err := json.Marshal(records)
	if err != nil {
		return model.IssueImportSource{}, ImportProblem(ImportFile, 1, 0, nil)
	}
	return model.IssueImportSource{SetSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), Count: len(records)}, nil
}

func (q IssueImport) Result() IssueImportResult {
	r := IssueImportResult{Count: len(q.Records), SHA256: q.FileSHA256, DryRun: q.DryRun}
	if q.DryRun {
		r.Outcome = "No issues imported."
	}
	for _, v := range q.Records {
		r.HistoryEntries += len(v.History)
	}
	return r
}

// AllowsIssueImport requires an explicit installation-wide administrator snapshot.
func (a Action) AllowsIssueImport() bool {
	return a.Actor != "" && a.Actor == a.Caller.Identity && a.Caller.Admin && len(a.Caller.Scope) == 0
}

// IssueImportFirstLine follows the count header on physical line 1.
const IssueImportFirstLine = 2

// Line reports a record's physical line in the import file.
func (q IssueImport) Line(index int) int { return IssueImportFirstLine + index }
