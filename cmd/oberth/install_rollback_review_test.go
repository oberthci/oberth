package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRollbackReviewShowsRecordAndExistingCommandWithoutWrites(t *testing.T) {
	f := signedMaintenanceFixture(t)
	upgradeSignedFixture(t, &f)
	record := filepath.Join(f.session.app.base, "installed", "rollback.json")
	before, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	history, applies := f.charts.History, len(f.charts.Applies)
	for _, args := range [][]string{{"rollback"}, {"rollback", "--dry-run"}, {"rollback", "--dry-run", "--yes"}} {
		app, out, diagnostic := f.rollbackApplication(t)
		code := app.run(t.Context(), args)
		if (code == 0) != (len(args) > 1) {
			t.Fatal("review result", code, diagnostic)
		}
		for _, text := range []string{record, "Source: v0.1.0; revision 1; schema", "Target: v0.2.0; revision 2; schema", "State: completed"} {
			if !strings.Contains(out.String(), text) {
				t.Error("missing record field", text, out)
			}
		}
		if len(args) == 1 && !strings.Contains(diagnostic.String(), "oberth rollback --dry-run") {
			t.Fatal("remedy cannot be run", diagnostic)
		}
		after, err := os.ReadFile(record)
		if err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(history, f.charts.History) || len(f.charts.Applies) != applies {
			t.Fatal("review wrote state", err)
		}
	}
}
