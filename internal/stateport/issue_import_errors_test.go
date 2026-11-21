package stateport

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestIssueImportMessagesAreWholeAndBounded(t *testing.T) {
	messages := importMessages()
	for key := noImportProblem + 1; key < importProblemCount; key++ {
		pair := messages[key]
		for i, limit := range []int{80, 120} {
			if err := publicerr.CheckMessage(pair[i], limit); err != nil || strings.HasSuffix(pair[i], "...") {
				t.Fatalf("%d: %v", key, err)
			}
		}
	}
}
