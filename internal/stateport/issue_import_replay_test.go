package stateport

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestIssueImportReplayRule(t *testing.T) {
	source := model.IssueImportSource{SetSHA256: strings.Repeat("a", 64), Count: 2}
	different := model.IssueImportSource{SetSHA256: strings.Repeat("b", 64), Count: 2}
	q := IssueImport{Records: []model.IssueImportRecord{{ID: 7}, {ID: 9}}}
	for name, stored := range map[string]map[int64]*model.IssueImportSource{
		"new": {}, "exact": {7: &source, 9: &source}, "mixed": {9: &source}, "native": {7: nil}, "different": {9: &different},
	} {
		t.Run(name, func(t *testing.T) {
			replay, err := q.CheckReplay(source, func(id int64) (model.Issue, bool, error) {
				v, ok := stored[id]
				return model.Issue{ID: id, Imported: v}, ok, nil
			})
			wantError := name != "new" && name != "exact"
			if (err != nil) != wantError || replay != (name == "exact") {
				t.Fatal(replay, err)
			}
			if err != nil && !strings.Contains(strings.Join(publicerr.Classify(err).Context, " "), "issue ") {
				t.Fatal("missing conflict number")
			}
		})
	}
	_, err := q.CheckReplay(source, func(int64) (model.Issue, bool, error) {
		return model.Issue{}, false, publicerr.New(publicerr.Unavailable, "Lookup failed.", "Retry the import.", nil)
	})
	if err == nil || publicerr.Classify(err).Code() != publicerr.Unavailable {
		t.Fatal("lookup failure became missing")
	}
}
