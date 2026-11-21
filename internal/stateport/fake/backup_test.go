package fake

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stateport/contracttest"
)

func TestBackupContract(t *testing.T) { contracttest.RunContract(t, contractFixture) }

func contractFixture(t *testing.T, index int) contracttest.Fixture {
	t.Helper()
	protocol := stateport.DefaultProtocol()
	schema := 17
	switch index {
	case 1:
		protocol.SchemaIdentity += "-other"
	case 2:
		schema++
	case 3:
		protocol.AuditDomain += "-other"
	}
	known := storedFixture{Content: "verified committed fixture", Head: stateport.AuditHead{ID: 3, SHA256: strings.Repeat("a", 64)}}
	database, err := json.Marshal(known)
	if err != nil {
		t.Fatal(err)
	}
	report := stateport.BackupReport{Schema: schema, AuditHead: known.Head, Integrity: true, ForeignKeys: true}
	backup, err := New(protocol, report, database)
	if err != nil {
		t.Fatal(err)
	}
	return contracttest.Fixture{
		Port: backup, Options: stateport.Options{Protocol: protocol, Schema: schema}, WantContent: known.Content, WantHead: known.Head,
		Read: func(t *testing.T, path string) (string, stateport.AuditHead, bool) {
			return readStoredFixture(t, backup, path)
		},
	}
}

type storedFixture struct {
	Content string
	Head    stateport.AuditHead
}

func readStoredFixture(t *testing.T, backup *Backup, path string) (string, stateport.AuditHead, bool) {
	t.Helper()
	backup.mu.Lock()
	body, found := backup.restored[path]
	body = bytes.Clone(body)
	backup.mu.Unlock()
	if !found {
		return "", stateport.AuditHead{}, false
	}
	var state storedFixture
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal("restored fixture cannot be read", err)
	}
	return state.Content, state.Head, true
}
