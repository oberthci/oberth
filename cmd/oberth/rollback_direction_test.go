package main

import (
	"strings"
	"testing"
)

func TestRollbackDryRunNamesTheReleaseItReturnsTo(t *testing.T) {
	f := signedMaintenanceFixture(t)
	upgradeSignedFixture(t, &f)
	app, out, diagnostic := f.rollbackApplication(t)
	if code := app.run(t.Context(), []string{"rollback", "--dry-run"}); code != 0 {
		t.Fatal("rollback review failed", code, diagnostic)
	}
	if !strings.Contains(out.String(), "Rollback: v0.2.0 -> v0.1.0\n") {
		t.Fatal("rollback destination is missing or reversed", out)
	}
}
