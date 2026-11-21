package stateport

import "testing"

func TestIssueImportLinesFollowCountHeader(t *testing.T) {
	q := IssueImport{}
	for index, line := range []int{2, 3, 4} {
		if q.Line(index) != line {
			t.Fatal("record line does not follow header", index, q.Line(index))
		}
	}
}
