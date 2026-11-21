package stateport

import "github.com/oberthci/oberth/internal/model"

// CheckReplay accepts all-new numbers or the complete immutable imported set.
// A missing number is represented by found=false; lookup failures remain failures.
func (q IssueImport) CheckReplay(source model.IssueImportSource, lookup func(int64) (model.Issue, bool, error)) (bool, error) {
	found, firstLine, firstID := 0, 0, int64(0)
	for i, v := range q.Records {
		issue, exists, err := lookup(v.ID)
		if err != nil {
			return false, ImportContext(err, q.Line(i), v.ID)
		}
		if !exists {
			continue
		}
		if issue.Imported == nil || *issue.Imported != source {
			return false, ImportProblem(ImportConflict, q.Line(i), v.ID, nil)
		}
		found++
		if firstLine == 0 {
			firstLine, firstID = q.Line(i), v.ID
		}
	}
	if found != 0 && found != len(q.Records) {
		return false, ImportProblem(ImportConflict, firstLine, firstID, nil)
	}
	return found == len(q.Records), nil
}
