package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestCommandProtocolConfiguresStartupAndAdministrativeReads(t *testing.T) {
	t.Setenv("OBERTH_SCHEMA_IDENTITY", "sample-oberth-schema-v1")
	t.Setenv("OBERTH_AUDIT_DOMAIN", "sample-oberth-audit-v1")
	t.Setenv("OBERTH_WITNESS_KEY_INFO", "sample-oberth-audit-witness-v1")
	ctx, err := withCommandProtocol(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if configuredWitnessKeyInfo(ctx) != "sample-oberth-audit-witness-v1" {
		t.Fatal("witness protocol not propagated")
	}
	path := filepath.Join(t.TempDir(), "instance.sqlite")
	db, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, witnessChainReset{}, witnessGenesisAdoption{}, func(db *store.Store) error { _, err := db.VerifyAuditState(ctx); return err })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "operator", Action: "fixture", ResourceType: "instance", Details: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openStartupDatabase(ctx, path, &staticStartupContinuity{}, witnessChainReset{}, witnessGenesisAdoption{}, func(db *store.Store) error { _, err := db.VerifyAuditState(ctx); return err })
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	admin, err := store.OpenAdminClient(ctx, path, configuredStoreOptions(ctx))
	if err != nil {
		t.Fatal(err)
	}
	admin.Close()
	// The command snapshot does not change halfway through a command.
	t.Setenv("OBERTH_AUDIT_DOMAIN", "different-domain")
	if configuredStoreOptions(ctx).Protocol.AuditDomain != "sample-oberth-audit-v1" {
		t.Fatal("protocol context followed changing environment")
	}
}

func TestCommandProtocolRejectsEmptyAndUnsafeSettings(t *testing.T) {
	for _, name := range []string{"OBERTH_SCHEMA_IDENTITY", "OBERTH_AUDIT_DOMAIN", "OBERTH_WITNESS_KEY_INFO"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "")
			if _, err := withCommandProtocol(context.Background()); err == nil {
				t.Fatal("explicit empty protocol setting accepted")
			}
		})
	}
}

func TestCommandProtocolRejectsEntirelyEmptyConfiguration(t *testing.T) {
	t.Setenv("OBERTH_SCHEMA_IDENTITY", "")
	t.Setenv("OBERTH_AUDIT_DOMAIN", "")
	if _, err := withCommandProtocol(context.Background()); err == nil {
		t.Fatal("explicit empty configuration silently selected defaults")
	}
}
