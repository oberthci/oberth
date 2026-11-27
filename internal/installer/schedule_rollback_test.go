package installer_test

import (
	"os"
	"strings"
	"testing"
)

func TestRollbackReviewNamesScheduleAdmissionCutover(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"v0.30.18", "v0.30.19"} {
		t.Run(version, func(t *testing.T) {
			_, m := upgradeWithServicePort(t, releaseFor(t, version, 6))
			lines, err := m.RollbackReview()
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Join(lines, "\n")
			for _, required := range []string{
				"Sealed schedule admissions from v0.30.19 may refuse after this downgrade.",
				"Pause schedules; finish or cancel every child and verify collection, then remove CronWorkflows before downgrading.",
				"Recreate schedules with the restored release before resuming them.",
			} {
				if strings.Contains(text, required) != (version == "v0.30.18") || len(required) > 120 {
					t.Fatal("rollback review lost the schedule admission boundary", text)
				}
			}
		})
	}
	guide, err := os.ReadFile("../../docs/install.md")
	if err != nil || !strings.Contains(string(guide), "verify collection before removing YAML declarations") {
		t.Fatal("rollback guide removes parents before child collection", err)
	}
}
