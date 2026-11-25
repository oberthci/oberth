package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

func TestRestoreDatabaseStreamPublishesOnceAndKeepsExistingBytes(t *testing.T) {
	t.Parallel()
	protocol := stateport.DefaultProtocol()
	clock := func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }
	source, err := store.Create(t.Context(), filepath.Join(t.TempDir(), "source.sqlite"), store.Options{Protocol: protocol, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	var archive bytes.Buffer
	expected, err := source.Snapshot(t.Context(), stateport.Options{Protocol: protocol, Schema: store.Schema}, &archive)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored.sqlite")
	args := []string{"restore-database", "--destination", destination, "--schema-identity", protocol.SchemaIdentity, "--audit-domain", protocol.AuditDomain}
	var out, diagnostic bytes.Buffer
	a := application{in: bytes.NewReader(archive.Bytes()), out: &out, diagnostic: &diagnostic}
	if code, ok := a.entry(t.Context(), args); !ok || code != 0 {
		t.Fatal("database stream did not restore", code, diagnostic.String())
	}
	var report stateport.BackupReport
	if json.Unmarshal(out.Bytes(), &report) != nil || report != expected {
		t.Fatal("database stream did not return its verified receipt")
	}
	checkExistingDatabaseRestore(t, destination, func() int {
		a.in = bytes.NewReader(archive.Bytes())
		code, _ := a.entry(t.Context(), args)
		return code
	})
}

func checkExistingDatabaseRestore(t *testing.T, destination string, replay func() int) {
	t.Helper()
	original, err := os.ReadFile(destination)
	if err != nil || len(original) == 0 {
		t.Fatal("database stream did not publish a database", err)
	}
	for range 2 {
		if replay() == 0 {
			t.Fatal("database stream accepted an existing destination")
		}
		current, err := os.ReadFile(destination)
		if err != nil || !bytes.Equal(current, original) {
			t.Fatal("database stream overwrote existing bytes", err)
		}
	}
}
