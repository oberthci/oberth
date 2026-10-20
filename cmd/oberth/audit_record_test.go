package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/store"
)

func TestAuditRecordAppendsLifecycleActionAfterGate(t *testing.T) {
	database := filepath.Join(t.TempDir(), "oberth.sqlite")
	initial, err := store.Open(context.Background(), database, configuredStoreOptions(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	var operations []string
	gate := func(_ context.Context, operation, _ string) error {
		operations = append(operations, operation)
		return nil
	}
	var output bytes.Buffer
	if err := runAuditRecordWithGate(context.Background(), []string{"backup.completed", "--database", database, "--details", `{"bytes":1}`}, &output, gate); err != nil {
		t.Fatal(err)
	}
	if len(operations) != 2 || operations[1] != "audit.record" || !strings.Contains(output.String(), "audit entry") {
		t.Fatalf("operations %v output %q", operations, output.String())
	}
	if err := runAuditRecordWithGate(context.Background(), []string{"secret_access.grant", "--database", database}, &output, gate); err == nil {
		t.Fatal("non-lifecycle action recorded")
	}
	denied := func(context.Context, string, string) error { return errors.New("gate closed") }
	if err := runAuditRecordWithGate(context.Background(), []string{"backup.completed", "--database", database}, &output, denied); err == nil {
		t.Fatal("recorded through a closed gate")
	}
}
