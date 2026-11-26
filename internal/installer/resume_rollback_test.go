package installer_test

import (
	"strings"
	"testing"
)

func TestResumeDowngradeReviewNamesCancellationAndOrphanCleanup(t *testing.T) {
	t.Parallel()
	_, m := upgradeWithServicePort(t, releaseFor(t, "v0.30.99", 6))
	lines, err := m.RollbackReview()
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(lines, "\n")
	for _, required := range []string{"cancel non-terminal resumed runs or wait for them to finish", "oberth.ci/record finalizer removed by hand"} {
		if !strings.Contains(text, required) {
			t.Fatal("rollback review lost resume prerequisite", text)
		}
	}
}
