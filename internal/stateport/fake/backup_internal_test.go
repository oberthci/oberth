package fake

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func fixture(t *testing.T) *Backup {
	t.Helper()
	backup, err := New(stateport.DefaultProtocol(), stateport.BackupReport{
		Schema: 1, AuditHead: stateport.AuditHead{ID: 1, SHA256: strings.Repeat("a", 64)}, Integrity: true, ForeignKeys: true,
	}, []byte("verified fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return backup
}

func TestFakeConstructorRefusesMissingVerifiedSeed(t *testing.T) {
	valid := fixture(t)
	for _, tc := range []struct {
		protocol stateport.Protocol
		report   stateport.BackupReport
		body     []byte
	}{
		{stateport.Protocol{}, valid.report, valid.database},
		{valid.protocol, stateport.BackupReport{}, valid.database},
		{valid.protocol, valid.report, nil},
	} {
		if _, err := New(tc.protocol, tc.report, tc.body); err == nil {
			t.Fatal("invalid seed accepted")
		}
	}
}

func TestFakeCopiesSeedBytes(t *testing.T) {
	valid := fixture(t)
	seed := bytes.Clone(valid.database)
	backup, err := New(valid.protocol, valid.report, seed)
	if err != nil {
		t.Fatal(err)
	}
	seed[0] ^= 1
	if bytes.Equal(seed, backup.database) {
		t.Fatal("seed data aliases the fake database")
	}
}
