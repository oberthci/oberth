package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The pre-upgrade backup can be encrypted to a certificate the operator
// manages, so an upgrade without a terminal never needs the backup-key
// ceremony (#11 of the fresh-install rehearsal).
func TestUpgradeBackupRecipientFlag(t *testing.T) {
	var out strings.Builder
	err := runUpgrade(t.Context(), []string{"--no-backup", "--backup-recipient", "x.pem"}, strings.NewReader(""), &out)
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "contradict") {
		t.Fatalf("--no-backup with --backup-recipient: %v", err)
	}
	err = runUpgrade(t.Context(), []string{"--yes"}, strings.NewReader(""), &out)
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "--rollback only") {
		t.Fatalf("--yes without --rollback: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.pem")
	_, err = takePreUpgradeBackup(t.Context(), t.TempDir(), missing, "", "oberth", strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "--backup-recipient") {
		t.Fatalf("a missing recipient certificate: %v", err)
	}
}
