package installer_test

import (
	"os"
	"strings"
	"testing"
)

func TestRollbackReviewNamesBackupReaderBoundary(t *testing.T) {
	t.Parallel()
	_, m := upgradeWithServicePort(t, releaseFor(t, "v0.30.18", 6))
	lines, err := m.RollbackReview()
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(lines, "\n")
	for _, required := range []string{
		"Backups taken by v0.30.19 need v0.30.19+ to verify their signed manifests.",
		"Keep a verified v0.30.19+ recovery CLI and the pre-upgrade backup.",
	} {
		if !strings.Contains(text, required) || len(required) > 120 {
			t.Fatal("rollback review hid the backup reader boundary", text)
		}
	}
	raw, err := os.ReadFile("../../docs/install.md")
	if err != nil || !strings.Contains(string(raw), "v0.30.18 cannot verify v0.30.19 signed manifests") {
		t.Fatal("restore guide hid the backup reader boundary", err)
	}
}
