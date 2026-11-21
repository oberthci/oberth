package stateport

import (
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestProtocolTokensAndZeroValuesGrantNothing(t *testing.T) {
	for _, value := range []string{"", "-start", "a b", "a'quote", "a\n", "λ", strings.Repeat("a", 129)} {
		protocol := DefaultProtocol()
		protocol.AuditDomain = value
		if protocol.Validate() == nil {
			t.Fatalf("invalid audit domain accepted: %q", value)
		}
		protocol = DefaultProtocol()
		protocol.SchemaIdentity = value
		if protocol.Validate() == nil {
			t.Fatalf("invalid schema identity accepted: %q", value)
		}
	}
	for _, value := range []string{"a", "A0:._-", strings.Repeat("a", 128)} {
		if err := (Protocol{value, value}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if (Options{}).Validate() == nil || (BackupReport{}).Validate() == nil {
		t.Fatal("zero options or unverified report accepted")
	}
}

func TestLimitDefaultAndHardBoundary(t *testing.T) {
	for _, tc := range []struct{ value, want int64 }{{0, DefaultMaxBytes}, {1, 1}, {HardMaxBytes, HardMaxBytes}} {
		got, err := (Options{MaxBytes: tc.value}).Limit()
		if err != nil || got != tc.want {
			t.Fatalf("limit(%d) = %d, %v", tc.value, got, err)
		}
	}
	for _, value := range []int64{-1, HardMaxBytes + 1} {
		if _, err := (Options{MaxBytes: value}).Limit(); err == nil {
			t.Fatal("invalid byte limit accepted")
		}
	}
}

func TestBackupReportRefusesEveryUnverifiedField(t *testing.T) {
	for name, change := range map[string]func(*BackupReport){
		"schema":           func(r *BackupReport) { r.Schema = 0 },
		"head_id":          func(r *BackupReport) { r.AuditHead.ID = -1 },
		"head_digest":      func(r *BackupReport) { r.AuditHead.SHA256 = "" },
		"uppercase_digest": func(r *BackupReport) { r.AuditHead.SHA256 = strings.Repeat("A", 64) },
		"integrity":        func(r *BackupReport) { r.Integrity = false },
		"foreign_keys":     func(r *BackupReport) { r.ForeignKeys = false },
	} {
		t.Run(name, func(t *testing.T) {
			report := BackupReport{Schema: 7, AuditHead: AuditHead{ID: 1, SHA256: strings.Repeat("a", 64)}, Integrity: true, ForeignKeys: true}
			if err := report.Validate(); err != nil {
				t.Fatal(err)
			}
			change(&report)
			var refusal *publicerr.Error
			if err := report.Validate(); !errors.As(err, &refusal) || refusal.Code() != publicerr.SchemaRefused {
				t.Fatalf("report verification = %v, want schema refusal", err)
			}
		})
	}
}
