package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/auditanchor"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/store"
)

func seedAdoptionDatabase(t *testing.T, path string, count int) model.AuditHead {
	t.Helper()
	ctx := context.Background()
	database, err := store.CreateGenesis(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var head model.AuditHead
	for i := 1; i <= count; i++ {
		action, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
			Actor: "agent@host", Action: "test", ResourceType: "startup",
			ResourceID: fmt.Sprintf("action-%d", i),
		})
		if err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
		head = model.AuditHead{ID: action.ID, SHA256: append([]byte(nil), action.SHA256...)}
	}
	if count == 0 {
		head, err = database.VerifyAuditState(ctx)
		if err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return head
}

// T3: AdoptRecordsAcknowledgmentAndSequenceOneIntent
func TestAdoptRecordsAcknowledgmentAndSequenceOneIntent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	head := seedAdoptionDatabase(t, path, 3)
	tips := &staticChainTips{}
	continuity := &staticStartupContinuity{}
	adoption := witnessGenesisAdoption{
		baselineID: head.ID, baselineSHA256: append([]byte(nil), head.SHA256...),
		chain: tips,
	}
	applied, database, err := adoptWitnessGenesis(ctx, path, continuity, adoption)
	if err != nil {
		t.Fatal(err)
	}
	if !applied || database == nil {
		t.Fatal("adoption not applied")
	}
	// Verify tail action is the acknowledgment.
	tail, err := database.TailAuditAction(ctx)
	if closeErr := database.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if tail.ID != head.ID+1 || tail.Action != witnessGenesisAdoptAction {
		t.Fatalf("tail = %+v, want ID=%d action=%s", tail, head.ID+1, witnessGenesisAdoptAction)
	}
	var details witnessGenesisAdoptDetails
	if err := json.Unmarshal([]byte(tail.Details), &details); err != nil {
		t.Fatal(err)
	}
	if details.BaselineAuditID != head.ID || details.BaselineSHA256 != hex.EncodeToString(head.SHA256) {
		t.Fatalf("details = %+v", details)
	}
	// Verify continuity holds exactly one intent.
	if len(continuity.intents) != 1 {
		t.Fatalf("intents = %d, want 1", len(continuity.intents))
	}
	intent := continuity.intents[0]
	if intent.Sequence != 1 || intent.AuditID != tail.ID || !bytes.Equal(intent.AuditSHA256, tail.SHA256) || intent.PreviousUUID != "" {
		t.Fatalf("intent = %+v", intent)
	}
	if tips.calls != 1 {
		t.Fatalf("identity check calls = %d, want 1", tips.calls)
	}
}

// T4: AdoptRequiresExactHead
func TestAdoptRequiresExactHead(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	head := seedAdoptionDatabase(t, path, 3)
	before := readAuthoritativeSQLiteFiles(t, path)

	t.Run("wrong ID", func(t *testing.T) {
		adoption := witnessGenesisAdoption{
			baselineID: head.ID + 1, baselineSHA256: append([]byte(nil), head.SHA256...),
			chain: &staticChainTips{},
		}
		applied, database, err := adoptWitnessGenesis(ctx, path, &staticStartupContinuity{}, adoption)
		if database != nil {
			_ = database.Close()
		}
		if applied || err == nil || !strings.Contains(err.Error(), "does not acknowledge") {
			t.Fatalf("wrong ID: applied=%v error=%v", applied, err)
		}
		assertSQLiteFilesEqual(t, before, readAuthoritativeSQLiteFiles(t, path))
	})

	t.Run("wrong hash", func(t *testing.T) {
		wrongHash := make([]byte, 32)
		copy(wrongHash, head.SHA256)
		wrongHash[0] ^= 0xff
		adoption := witnessGenesisAdoption{
			baselineID: head.ID, baselineSHA256: wrongHash,
			chain: &staticChainTips{},
		}
		applied, database, err := adoptWitnessGenesis(ctx, path, &staticStartupContinuity{}, adoption)
		if database != nil {
			_ = database.Close()
		}
		if applied || err == nil || !strings.Contains(err.Error(), "does not acknowledge") {
			t.Fatalf("wrong hash: applied=%v error=%v", applied, err)
		}
	})
}

// T5: AdoptRefusesWhenIdentityHasPublicHistory
func TestAdoptRefusesWhenIdentityHasPublicHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	head := seedAdoptionDatabase(t, path, 3)
	tips := &staticChainTips{
		tip: model.AuditWitness{
			UUID: strings.Repeat("a", 64), LogIndex: 42,
			IntegratedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
			AuditID:      5, AuditSHA256: bytes.Repeat([]byte{0x11}, 32),
		},
		found: true,
	}
	adoption := witnessGenesisAdoption{
		baselineID: head.ID, baselineSHA256: append([]byte(nil), head.SHA256...),
		chain: tips,
	}
	applied, database, err := adoptWitnessGenesis(ctx, path, &staticStartupContinuity{}, adoption)
	if database != nil {
		_ = database.Close()
	}
	if applied || err == nil || !strings.Contains(err.Error(), "already has published Rekor history") {
		t.Fatalf("public history: applied=%v error=%v", applied, err)
	}
}

// T6: AdoptFailsClosedWhenRekorUnavailable
func TestAdoptFailsClosedWhenRekorUnavailable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	head := seedAdoptionDatabase(t, path, 3)
	rekorErr := fmt.Errorf("connection refused: %w", auditanchor.ErrWitnessUnavailable)
	tips := &staticChainTips{err: rekorErr}
	adoption := witnessGenesisAdoption{
		baselineID: head.ID, baselineSHA256: append([]byte(nil), head.SHA256...),
		chain: tips,
	}
	_, database, err := adoptWitnessGenesis(ctx, path, &staticStartupContinuity{}, adoption)
	if database != nil {
		_ = database.Close()
	}
	if err == nil || !errors.Is(err, auditanchor.ErrWitnessUnavailable) {
		t.Fatalf("rekor unavailable: error=%v, want wrapped ErrWitnessUnavailable", err)
	}
	// Verify the degraded-startup carve-out: openStartupDatabase with the adoption flag
	// must NOT defer recovery.
	database, openErr := openStartupDatabase(ctx, path, &staticStartupContinuity{}, witnessChainReset{},
		witnessGenesisAdoption{baselineID: head.ID, baselineSHA256: append([]byte(nil), head.SHA256...), chain: tips},
		func(inspection *store.Store) error {
			return rekorErr
		})
	if database != nil {
		_ = database.Close()
	}
	if !errors.Is(openErr, auditanchor.ErrWitnessUnavailable) {
		t.Fatalf("degraded startup should fail closed with adoption flag: error=%v", openErr)
	}
}

// T7: AdoptIgnoredWhenContinuityExists
func TestAdoptIgnoredWhenContinuityExists(t *testing.T) {
	ctx := context.Background()

	t.Run("non-empty intents", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth.sqlite")
		head := seedAdoptionDatabase(t, path, 3)
		tips := &staticChainTips{}
		continuity := &staticStartupContinuity{intents: []model.AuditWitnessIntent{
			{Sequence: 1, AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...)},
		}}
		adoption := witnessGenesisAdoption{
			baselineID: head.ID, baselineSHA256: append([]byte(nil), head.SHA256...),
			chain: tips,
		}
		applied, database, err := adoptWitnessGenesis(ctx, path, continuity, adoption)
		if database != nil {
			_ = database.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		if applied {
			t.Fatal("adoption should be ignored with existing intents")
		}
		if tips.calls != 0 {
			t.Fatalf("identity check calls = %d, want 0 (no Rekor consultation)", tips.calls)
		}
	})

	t.Run("non-empty pinned", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth.sqlite")
		head := seedAdoptionDatabase(t, path, 3)
		tips := &staticChainTips{}
		continuity := &staticStartupContinuity{pinned: []model.AuditWitness{
			{UUID: "existing-witness"},
		}}
		adoption := witnessGenesisAdoption{
			baselineID: head.ID, baselineSHA256: append([]byte(nil), head.SHA256...),
			chain: tips,
		}
		applied, database, err := adoptWitnessGenesis(ctx, path, continuity, adoption)
		if database != nil {
			_ = database.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		if applied {
			t.Fatal("adoption should be ignored with existing pinned witnesses")
		}
		if tips.calls != 0 {
			t.Fatalf("identity check calls = %d, want 0", tips.calls)
		}
	})
}

// T8: AdoptResumesAfterCrashBeforeIntent
func TestAdoptResumesAfterCrashBeforeIntent(t *testing.T) {
	ctx := context.Background()

	// Seed a database, then adopt (writing the acknowledgment), then close without
	// the intent. Then resume.
	seedAndAdopt := func(t *testing.T) (string, model.AuditHead, model.AuditAction) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "oberth.sqlite")
		head := seedAdoptionDatabase(t, path, 3)
		tips := &staticChainTips{}
		continuity := &staticStartupContinuity{}
		adoption := witnessGenesisAdoption{
			baselineID: head.ID, baselineSHA256: append([]byte(nil), head.SHA256...),
			chain: tips,
		}
		applied, database, err := adoptWitnessGenesis(ctx, path, continuity, adoption)
		if err != nil || !applied {
			t.Fatalf("initial adoption: applied=%v err=%v", applied, err)
		}
		tail, err := database.TailAuditAction(ctx)
		if err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
		_ = database.Close()
		return path, head, tail
	}

	t.Run("flag names original baseline", func(t *testing.T) {
		path, head, _ := seedAndAdopt(t)
		tips := &staticChainTips{}
		continuity := &staticStartupContinuity{} // empty = no intent survived crash
		adoption := witnessGenesisAdoption{
			baselineID: head.ID, baselineSHA256: append([]byte(nil), head.SHA256...),
			chain: tips,
		}
		applied, database, err := adoptWitnessGenesis(ctx, path, continuity, adoption)
		if err != nil || !applied {
			t.Fatalf("resume with baseline flag: applied=%v err=%v", applied, err)
		}
		_ = database.Close()
		if tips.calls != 1 {
			t.Fatalf("identity check calls = %d, want 1", tips.calls)
		}
		if len(continuity.intents) != 1 {
			t.Fatalf("intents = %d, want 1", len(continuity.intents))
		}
	})

	t.Run("flag names acknowledgment head", func(t *testing.T) {
		path, _, tail := seedAndAdopt(t)
		tips := &staticChainTips{}
		continuity := &staticStartupContinuity{}
		adoption := witnessGenesisAdoption{
			baselineID: tail.ID, baselineSHA256: append([]byte(nil), tail.SHA256...),
			chain: tips,
		}
		applied, database, err := adoptWitnessGenesis(ctx, path, continuity, adoption)
		if err != nil || !applied {
			t.Fatalf("resume with ack head flag: applied=%v err=%v", applied, err)
		}
		_ = database.Close()
		if len(continuity.intents) != 1 {
			t.Fatalf("intents = %d, want 1", len(continuity.intents))
		}
	})
}

// T9: AdoptResumeRefusesUnrelatedBaseline
func TestAdoptResumeRefusesUnrelatedBaseline(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	head := seedAdoptionDatabase(t, path, 3)
	// First adopt to write the acknowledgment.
	tips := &staticChainTips{}
	continuity := &staticStartupContinuity{}
	adoption := witnessGenesisAdoption{
		baselineID: head.ID, baselineSHA256: append([]byte(nil), head.SHA256...),
		chain: tips,
	}
	applied, database, err := adoptWitnessGenesis(ctx, path, continuity, adoption)
	if err != nil || !applied {
		t.Fatalf("initial adoption: applied=%v err=%v", applied, err)
	}
	_ = database.Close()

	// Resume with an unrelated value.
	wrongAdoption := witnessGenesisAdoption{
		baselineID: 999, baselineSHA256: bytes.Repeat([]byte{0xbb}, 32),
		chain: &staticChainTips{},
	}
	_, database, err = adoptWitnessGenesis(ctx, path, &staticStartupContinuity{}, wrongAdoption)
	if database != nil {
		_ = database.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "refuse witness genesis resume") {
		t.Fatalf("unrelated baseline error = %v", err)
	}
}

// T10: AdoptRefusesAbsentDatabase
func TestAdoptRefusesAbsentDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	adoption := witnessGenesisAdoption{
		baselineID: 1, baselineSHA256: bytes.Repeat([]byte{0xaa}, 32),
		chain: &staticChainTips{},
	}
	database, err := openStartupDatabase(context.Background(), path, &staticStartupContinuity{},
		witnessChainReset{}, adoption, func(*store.Store) error {
			t.Fatal("verifier ran for absent database")
			return nil
		})
	if database != nil {
		_ = database.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "requires an existing database") {
		t.Fatalf("absent database error = %v", err)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("database created despite adoption refusal: %v", statErr)
	}
}

// T11: Serve-options validation
func TestServeOptionsWitnessGenesisValidation(t *testing.T) {
	t.Run("valid pattern", func(t *testing.T) {
		options := minimalValidServeOptions()
		options.auditRekorURL = "https://rekor.example.test"
		options.acceptWitnessGenesis = "12:" + strings.Repeat("ab", 32)
		if err := validateServeOptions(options); err != nil {
			t.Fatalf("valid witness genesis rejected: %v", err)
		}
	})

	t.Run("reject ID 0", func(t *testing.T) {
		options := minimalValidServeOptions()
		options.auditRekorURL = "https://rekor.example.test"
		options.acceptWitnessGenesis = "0:" + strings.Repeat("ab", 32)
		if err := validateServeOptions(options); err == nil {
			t.Fatal("ID 0 should be rejected by the pattern")
		}
	})

	t.Run("reject missing colon", func(t *testing.T) {
		options := minimalValidServeOptions()
		options.auditRekorURL = "https://rekor.example.test"
		options.acceptWitnessGenesis = strings.Repeat("ab", 32)
		if err := validateServeOptions(options); err == nil {
			t.Fatal("missing colon should be rejected")
		}
	})

	t.Run("reject uppercase hex", func(t *testing.T) {
		options := minimalValidServeOptions()
		options.auditRekorURL = "https://rekor.example.test"
		options.acceptWitnessGenesis = "1:" + strings.Repeat("AB", 32)
		if err := validateServeOptions(options); err == nil {
			t.Fatal("uppercase hex should be rejected")
		}
	})

	t.Run("requires rekor URL", func(t *testing.T) {
		options := minimalValidServeOptions()
		options.acceptWitnessGenesis = "1:" + strings.Repeat("ab", 32)
		if err := validateServeOptions(options); err == nil || !strings.Contains(err.Error(), "requires the external Rekor witness") {
			t.Fatalf("missing rekor URL error = %v", err)
		}
	})

	t.Run("mutual exclusion with reset", func(t *testing.T) {
		options := minimalValidServeOptions()
		options.auditRekorURL = "https://rekor.example.test"
		options.acceptWitnessGenesis = "1:" + strings.Repeat("ab", 32)
		options.acceptWitnessChainReset = strings.Repeat("cc", 32)
		if err := validateServeOptions(options); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("mutual exclusion error = %v", err)
		}
	})
}

func minimalValidServeOptions() serveOptions {
	return serveOptions{
		dataRoot:                "/data",
		database:                "/data/oberth.sqlite",
		sshListen:               ":2222",
		httpsListen:             ":8443",
		namespace:               "oberth",
		runnerImagePrefixes:     "golang:",
		maxConcurrent:           3,
		gitCommandTimeout:       10 * time.Minute,
		gcInterval:              24 * time.Hour,
		auditAnchorInterval:     10 * time.Minute,
		auditAnchorMaxAge:       30 * time.Minute,
		argoNamespace:           "oberth-argo",
		argoPipelineAccount:     "oberth-argo-pipeline",
		argoCredentialedAccount: "oberth-argo-credentialed",
		argoCISecretsAccount:    "oberth-argo-ci-secrets",
		argoExecutorAccount:     "oberth-argo-executor",
		argoWorkflowTimeout:     12 * time.Hour,
		argoWorkflowTTL:         3600,
		secretStoreAuthMount:    secretstore.DefaultAuthMountPath,
		secretStoreKVMount:      secretstore.DefaultKVMount,
	}
}

// T12: AdoptPhase1RefusesTSAAnchoredHistory
func TestAdoptPhase1RefusesTSAAnchoredHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	head := seedAdoptionDatabase(t, path, 3)

	// Seed a TSA anchor row.
	database, err := store.OpenCurrent(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...),
		TSAURL: "https://tsa.example.test", Receipt: []byte("fake-receipt"),
		AnchoredAt: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	adoption := witnessGenesisAdoption{
		baselineID: head.ID, baselineSHA256: append([]byte(nil), head.SHA256...),
		chain: &staticChainTips{},
	}
	_, db, err := adoptWitnessGenesis(ctx, path, &staticStartupContinuity{}, adoption)
	if db != nil {
		_ = db.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "not supported yet") {
		t.Fatalf("TSA-anchored error = %v", err)
	}
}

// T13: AdoptZeroValueChangesNothing
func TestAdoptZeroValueChangesNothing(t *testing.T) {
	ctx := context.Background()

	t.Run("genesis create unchanged", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth.sqlite")
		database, err := openStartupDatabase(ctx, path, &staticStartupContinuity{},
			witnessChainReset{}, witnessGenesisAdoption{}, func(*store.Store) error {
				t.Fatal("verifier ran for fresh genesis")
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
		_ = database.Close()
	})

	t.Run("existing verify unchanged", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth.sqlite")
		_ = seedAdoptionDatabase(t, path, 3)
		verified := false
		database, err := openStartupDatabase(ctx, path, &staticStartupContinuity{},
			witnessChainReset{}, witnessGenesisAdoption{}, func(inspection *store.Store) error {
				verified = true
				_, err := inspection.VerifyAuditState(ctx)
				return err
			})
		if err != nil {
			t.Fatal(err)
		}
		_ = database.Close()
		if !verified {
			t.Fatal("existing verifier not called")
		}
	})
}

// T14: AdoptThenSteadyStateSurvivesFlagRemovalAndRestart
func TestAdoptThenSteadyStateSurvivesFlagRemovalAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	head := seedAdoptionDatabase(t, path, 3)
	continuity := &staticStartupContinuity{}
	tips := &staticChainTips{}
	adoption := witnessGenesisAdoption{
		baselineID: head.ID, baselineSHA256: append([]byte(nil), head.SHA256...),
		chain: tips,
	}
	applied, database, err := adoptWitnessGenesis(ctx, path, continuity, adoption)
	if err != nil || !applied {
		t.Fatalf("adoption: applied=%v err=%v", applied, err)
	}
	_ = database.Close()

	// Reopen with zero-value adoption (flag removed).
	database, err = openStartupDatabase(ctx, path, continuity, witnessChainReset{},
		witnessGenesisAdoption{}, func(inspection *store.Store) error {
			_, err := inspection.VerifyAuditState(ctx)
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	_ = database.Close()
}

// T15: AdoptStaleFlagAfterSuccessIsNoOp
func TestAdoptStaleFlagAfterSuccessIsNoOp(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	head := seedAdoptionDatabase(t, path, 3)
	continuity := &staticStartupContinuity{}
	tips := &staticChainTips{}
	adoption := witnessGenesisAdoption{
		baselineID: head.ID, baselineSHA256: append([]byte(nil), head.SHA256...),
		chain: tips,
	}
	applied, database, err := adoptWitnessGenesis(ctx, path, continuity, adoption)
	if err != nil || !applied {
		t.Fatalf("adoption: applied=%v err=%v", applied, err)
	}

	// Add more actions to advance the chain.
	for i := 0; i < 2; i++ {
		if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
			Actor: "agent@host", Action: "test", ResourceType: "post-adopt", ResourceID: fmt.Sprintf("extra-%d", i),
		}); err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
	}
	_ = database.Close()

	// Stale flag still set, chain advanced, continuity non-empty (from the adoption).
	staleTips := &staticChainTips{}
	applied, database, err = adoptWitnessGenesis(ctx, path, continuity, witnessGenesisAdoption{
		baselineID: head.ID, baselineSHA256: append([]byte(nil), head.SHA256...),
		chain: staleTips,
	})
	if database != nil {
		_ = database.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("stale flag should be a no-op")
	}
	if staleTips.calls != 0 {
		t.Fatalf("stale flag consulted Rekor %d times", staleTips.calls)
	}
}

// T16: AuditHeadPrintsAcknowledgmentString
func TestAuditHeadPrintsAcknowledgmentString(t *testing.T) {
	t.Run("seeded database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth.sqlite")
		head := seedAdoptionDatabase(t, path, 5)
		var output bytes.Buffer
		err := runAuditHead(context.Background(), []string{"--database", path}, &output)
		if err != nil {
			t.Fatal(err)
		}
		expected := fmt.Sprintf("%d:%s\n", head.ID, hex.EncodeToString(head.SHA256))
		if output.String() != expected {
			t.Fatalf("output = %q, want %q", output.String(), expected)
		}
	})

	t.Run("empty chain", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth.sqlite")
		_ = seedAdoptionDatabase(t, path, 0)
		var output bytes.Buffer
		err := runAuditHead(context.Background(), []string{"--database", path}, &output)
		if err != nil {
			t.Fatal(err)
		}
		expected := "0:" + strings.Repeat("0", 64) + "\n"
		if output.String() != expected {
			t.Fatalf("output = %q, want %q", output.String(), expected)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		var output bytes.Buffer
		err := runAuditHead(context.Background(), []string{"--database", filepath.Join(t.TempDir(), "nonexistent.sqlite")}, &output)
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})
}
