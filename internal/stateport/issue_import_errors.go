package stateport

import (
	"fmt"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
)

// ImportProblemKey identifies one bounded import refusal.
type ImportProblemKey uint8

const (
	noImportProblem ImportProblemKey = iota
	ImportHeader
	ImportCount
	ImportFile
	ImportFileSize
	ImportLineSize
	ImportUTF8
	ImportDuplicate
	ImportID
	ImportTitle
	ImportState
	ImportKind
	ImportWorkStatus
	ImportOccurrences
	ImportCreatedAt
	ImportUpdatedAt
	ImportClosedAt
	ImportScope
	ImportHistory
	ImportConflict
	importProblemCount
)

func importMessages() [importProblemCount][2]string {
	return [importProblemCount][2]string{
		ImportHeader:      {"The import file header is invalid.", "Start with a JSON object containing only count, the positive number of issue records."},
		ImportCount:       {"The issue count does not match the file header.", "Transfer or export the complete numbered file before retrying."},
		ImportFile:        {"The issue import file is empty or invalid.", "Supply a nonempty JSON Lines file with a valid digest."},
		ImportFileSize:    {"The issue import file exceeds 1048576 bytes.", "Use several files with disjoint numbers, each within the file and encoded request limits."},
		ImportLineSize:    {"The issue import line exceeds 131072 bytes.", "Reduce the record to fit the line and complete issue limits before importing."},
		ImportUTF8:        {"The issue import line has invalid UTF-8.", "Encode the source text as UTF-8 before exporting again."},
		ImportDuplicate:   {"The issue number occurs twice in this file.", "Keep each issue number in exactly one record."},
		ImportID:          {"The issue number is outside the import range.", "Use an integer from 1 through 9223372036854775806, leaving room for the next native issue."},
		ImportTitle:       {"The imported issue title is blank.", "Supply a nonblank title."},
		ImportState:       {"The imported issue state is invalid.", "Use open or closed."},
		ImportKind:        {"Only manual issues can be imported.", "Export CI issues as manual issues with their original selectors in a provenance sentence."},
		ImportWorkStatus:  {"The imported issue work status is invalid.", "Use todo, active, waiting or review."},
		ImportOccurrences: {"The imported occurrence count is invalid.", "Supply a positive integer occurrence count."},
		ImportCreatedAt:   {"The imported issue creation time is missing.", "Supply a nonzero RFC3339 created_at timestamp."},
		ImportUpdatedAt:   {"The imported issue update time is missing.", "Supply a nonzero RFC3339 updated_at timestamp or omit it to derive it from source times."},
		ImportClosedAt:    {"The imported issue closing time does not match its state.", "Supply a nonzero closed_at for closed issues; omit it for open issues."},
		ImportScope:       {"The imported upstream or repository scope is invalid.", "Use a registered 1..53-character upstream, or map with --upstream <local>; keep the repository in its upstream."},
		ImportHistory:     {"The imported history entry is invalid.", "Use comment, append or state entries with a timestamp; only state entries carry open or closed."},
		ImportConflict:    {"An issue number in this file already exists in the installation.", "Numbers cannot be kept here; use an issue-free installation or replay the exact original file with the same --upstream."},
	}
}

// ImportProblem keeps file location and outcome outside bounded refusal sentences.
func ImportProblem(key ImportProblemKey, line int, id int64, cause error) error {
	m := importMessages()[key]
	code := publicerr.InvalidInput
	if key == ImportConflict {
		code = publicerr.Conflict
	}
	return ImportContext(publicerr.New(code, m[0], m[1], cause), line, id)
}

// ImportContext marks a known refusal before application or after rollback.
func ImportContext(err error, line int, id int64) error {
	if err == nil {
		return nil
	}
	p := publicerr.Classify(err)
	p.Context = slices.Clone(p.Context)
	if line > 0 && !slices.ContainsFunc(p.Context, func(s string) bool { return strings.HasPrefix(s, "line ") }) {
		where := fmt.Sprintf("line %d", line)
		if id != 0 {
			where += fmt.Sprintf(", issue %d", id)
		}
		p.Context = append(p.Context, where)
	}
	p.Context = append(p.Context, "No issues imported.")
	return p
}
