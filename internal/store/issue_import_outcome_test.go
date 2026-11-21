package store

import (
	"github.com/oberthci/oberth/internal/publicerr"
	"strings"
	"testing"
)

func TestIssueImportUnconfirmedCommitRequiresExactRetry(t *testing.T) {
	err := importOutcome(commitFailure{publicerr.New(publicerr.Unavailable, "Commit reply unavailable.", "Check the import receipt.", nil)})
	context := strings.Join(publicerr.Classify(err).Context, " ")
	if !strings.Contains(context, "run the same file again") || strings.Contains(context, "No issues imported.") {
		t.Fatal(context)
	}
}
