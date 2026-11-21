package cronsync

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

// Triggering an entry that is not admitted names that entry's reason and its
// remedy, for every reason reconciliation can leave on an entry.
func TestNotAdmittedNamesTheEntryReasonAndRemedy(t *testing.T) {
	for id, m := range messages() {
		var status Status
		refusalStatus(&status, &refusal{problem(diagnostic(id), nil), diagnostic(id)})
		failure := NotAdmitted(status)
		if failure.Reason() != "Schedule not admitted: "+m.reason+"." || failure.Remedy() != m.remedy || failure.Code() != publicerr.Conflict {
			t.Fatal("trigger refusal lost the entry's reason", id, failure.Reason(), failure.Remedy())
		}
		if !strings.Contains(failure.Detail, m.reason) {
			t.Fatal("trigger refusal lost the entry's detail", failure.Detail)
		}
	}
	missing := Status{}
	statusError(&missing, branchMissing, "entry \"nightly\": field \"branch\" has value \"nightly\"")
	if failure := NotAdmitted(missing); failure.Reason() != "Schedule not admitted: schedule branch not found." ||
		!strings.Contains(failure.Remedy(), "git push oberth <branch>") || !strings.Contains(failure.Detail, `"nightly"`) {
		t.Fatal("branch refusal does not say what to run", failure.Reason(), failure.Remedy(), failure.Detail)
	}
	if failure := NotAdmitted(Status{SyncSuspended: true}); failure.Reason() != "Schedule not admitted: "+messages()[pendingSync].reason+"." {
		t.Fatal("suspended entry without a reason", failure.Reason())
	}
}
