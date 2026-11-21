package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func requireCode(t *testing.T, err error, code publicerr.Code) {
	t.Helper()
	var refusal *publicerr.Error
	if !errors.As(err, &refusal) || refusal.Code() != code {
		t.Fatalf("error %v, want %s", err, code)
	}
}

func requireOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func replaceAuditDetail(t *testing.T, s *Store) {
	t.Helper()
	var trigger string
	requireOK(t, s.writer.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name='audit_immutable'`).Scan(&trigger))
	for _, statement := range []string{`DROP TRIGGER audit_immutable`, `UPDATE audit SET details='"changed"' WHERE id=1`, trigger} {
		execute(t, s.writer, statement)
	}
}

// Covers: REQ-C7-004
func TestLocalAuditChainContract(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "reader"}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
	_, _, err := s.Audit(t.Context(), 0, 200)
	requireOK(t, err)
	replaceAuditDetail(t, s)
	// Corruption must not make ordinary mutations scan the existing chain.
	requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "reader"}, stateport.Upstream{Name: "second", Org: "other", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
	_, _, err = s.Audit(t.Context(), 0, 200)
	requireCode(t, err, publicerr.SchemaRefused)
	var output bytes.Buffer
	_, err = s.Snapshot(t.Context(), backupOptions(s), &output)
	requireCode(t, err, publicerr.SchemaRefused)
	if output.Len() != 0 {
		t.Fatal("unverified backup emitted")
	}
	stage := filepath.Join(t.TempDir(), "corrupt.sqlite")
	requireOK(t, snapshotDatabase(t.Context(), s.path, stage, stateport.DefaultMaxBytes))
	var incoming bytes.Buffer
	// Recompute the member hash to prove database verification, not framing, refuses it.
	report := stateport.BackupReport{Schema: Schema, AuditHead: independentHead(t, s.readers), Integrity: true, ForeignKeys: true}
	requireOK(t, emitArchive(t.Context(), archiveOutput{stage, backupOptions(s), report, &incoming}))
	_, err = backupForTest(t, s).Restore(t.Context(), stateport.RestoreRequest{Options: backupOptions(s), Destination: filepath.Join(t.TempDir(), "restore.sqlite")}, bytes.NewReader(incoming.Bytes()))
	requireCode(t, err, publicerr.SchemaRefused)
}

// Covers: REQ-C7-009
func TestNonwideningDurableIdentityContract(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
	key := stateport.Key{Fingerprint: "key-one", Identity: "agent", Kind: "agent", ParkOnPush: true, Scope: []string{"origin"}}
	requireOK(t, s.RegisterKey(t.Context(), stateport.Action{Actor: "operator"}, key))
	digest := strings.Repeat("a", 64)
	requireOK(t, s.AddCredential(t.Context(), stateport.Action{Actor: "operator"}, stateport.Credential{Digest: digest, Fingerprint: key.Fingerprint, ExpiresAt: s.options.Clock().Add(time.Hour)}))
	key.Scope = nil
	key.Admin = true
	requireOK(t, s.RegisterKey(t.Context(), stateport.Action{Actor: "operator"}, key))
	actor, err := s.Authenticate(t.Context(), 0, digest)
	requireOK(t, err)
	if actor.Admin || len(actor.Scope) != 1 || actor.Scope[0] != "origin" {
		t.Fatal("registration widened authority", actor)
	}
	requireOK(t, s.RemoveUpstream(t.Context(), stateport.Action{Actor: "operator"}, "origin"))
	requireCode(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "origin", Org: "another", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}), publicerr.Conflict)
	actor, err = s.Authenticate(t.Context(), 1, digest)
	requireOK(t, err)
	if actor.CanTouchRepository("other/team/repo") || len(actor.Scope) != 1 {
		t.Fatal("deletion widened scope")
	}
	requireOK(t, s.SetKeyScope(t.Context(), stateport.Action{Actor: "operator"}, key.Fingerprint, nil))
	actor, err = s.Authenticate(t.Context(), 0, digest)
	requireOK(t, err)
	if len(actor.Scope) != 0 {
		t.Fatal("explicit scope change did not apply")
	}
	requireOK(t, s.RevokeKey(t.Context(), stateport.Action{Actor: "operator"}, key.Fingerprint))
	requireOK(t, s.RegisterKey(t.Context(), stateport.Action{Actor: "operator"}, key))
	_, err = s.Authenticate(t.Context(), 0, digest)
	requireCode(t, err, publicerr.Forbidden)
	requireCode(t, s.AddCredential(t.Context(), stateport.Action{Actor: "operator"}, stateport.Credential{Digest: strings.Repeat("b", 64), Fingerprint: key.Fingerprint, ExpiresAt: s.options.Clock().Add(time.Hour)}), publicerr.Forbidden)
}

// Covers: REQ-C7-011
func TestTransactionalEvidenceUpdatesContract(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	issue, err := s.CreateIssue(t.Context(), stateport.Action{Actor: "one"}, model.Issue{Kind: model.IssueManual, Title: "inspect", Body: "original"})
	requireOK(t, err)
	second, err := Open(t.Context(), s.path, s.options)
	requireOK(t, err)
	t.Cleanup(func() { requireOK(t, second.Close()) })
	var group sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	for index, writer := range []*Store{s, second} {
		group.Go(func() {
			<-start
			title := []string{"first", "second"}[index]
			_, err := writer.UpdateIssue(t.Context(), stateport.Action{Actor: "one"}, model.IssuePatch{ID: issue.ID, ExpectedRevision: &issue.Revision, Title: &title})
			results <- err
		})
	}
	close(start)
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			requireCode(t, err, publicerr.Conflict)
		}
	}
	if successes != 1 {
		t.Fatal("CAS lost an update", successes)
	}
	current, err := s.Issue(t.Context(), issue.ID)
	requireOK(t, err)
	requireOK(t, s.ChangeIssueLock(t.Context(), stateport.Action{Actor: "one"}, stateport.IssueLockRequest{ID: issue.ID, Owner: "one"}))
	before, err := s.Issue(t.Context(), issue.ID)
	requireOK(t, err)
	wrong := "new body"
	_, err = s.UpdateIssue(t.Context(), stateport.Action{Actor: "one"}, model.IssuePatch{ID: issue.ID, ExpectedRevision: &issue.Revision, Body: &wrong})
	requireCode(t, err, publicerr.Conflict)
	after, err := s.Issue(t.Context(), issue.ID)
	requireOK(t, err)
	if encode(before) != encode(after) {
		t.Fatal("stale edit renewed lock or changed metadata")
	}
	for _, table := range []string{"issues", "audit"} {
		execute(t, s.writer, `CREATE TRIGGER fail_write BEFORE `+map[string]string{"issues": "UPDATE", "audit": "INSERT"}[table]+` ON `+table+` BEGIN SELECT RAISE(ABORT,'injected write failure'); END`)
		_, err = s.UpdateIssue(t.Context(), stateport.Action{Actor: "one"}, model.IssuePatch{ID: issue.ID, ExpectedRevision: &current.Revision, Body: &wrong})
		if err == nil {
			t.Fatal("injected failure did not fire", table)
		}
		execute(t, s.writer, `DROP TRIGGER fail_write`)
		got, err := s.Issue(t.Context(), issue.ID)
		requireOK(t, err)
		if encode(got) != encode(before) {
			t.Fatal("partial transaction committed", table)
		}
	}
}

// Covers: REQ-C7-019
func TestNonblockingAuthenticationTelemetryContract(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	requireOK(t, s.RegisterKey(t.Context(), stateport.Action{Actor: "operator"}, stateport.Key{Fingerprint: "key", Identity: "agent", Kind: "agent", ParkOnPush: true}))
	digest := strings.Repeat("c", 64)
	requireOK(t, s.AddCredential(t.Context(), stateport.Action{Actor: "operator"}, stateport.Credential{Digest: digest, Fingerprint: "key", ExpiresAt: s.options.Clock().Add(time.Hour)}))
	// An unavailable optional consumer makes overload deterministic.
	s.stopUses()
	<-s.usesDone
	held, err := s.writer.BeginTx(t.Context(), nil)
	requireOK(t, err)
	defer func() { _ = held.Rollback() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		for range 128 {
			if _, err := s.Authenticate(ctx, 0, digest); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		requireOK(t, err)
	case <-ctx.Done():
		t.Fatal("optional telemetry blocked authentication")
	}
	if len(s.uses) != cap(s.uses) {
		t.Fatal("overload setup did not occupy bounded telemetry queue")
	}
	requireOK(t, held.Rollback())
}

func TestMessagesAndRestartReasonsAreComplete(t *testing.T) {
	t.Parallel()
	for kind := range diagnostics() {
		_, reason, remedy := message(kind)
		requireOK(t, publicerr.CheckMessage(reason, 80))
		requireOK(t, publicerr.CheckMessage(remedy, 120))
	}
	for _, reason := range []string{stateport.ReasonRestart, stateport.ReasonBranchDeleted} {
		requireOK(t, publicerr.CheckMessage(reason, 40))
	}
}

func TestExactSchemaAndMissingAdminDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "absent.sqlite")
	_, err := Open(t.Context(), path, testOptions())
	requireCode(t, err, publicerr.Unavailable)
	s, err := Create(t.Context(), path, testOptions())
	requireOK(t, err)
	requireOK(t, s.Close())
	options := testOptions()
	options.Protocol.SchemaIdentity += "-other"
	_, err = Open(t.Context(), path, options)
	requireCode(t, err, publicerr.SchemaRefused)
	db := pool(path, "rw", testOptions(), 1)
	t.Cleanup(func() { requireOK(t, db.Close()) })
	execute(t, db, `CREATE TABLE unexpected(id INTEGER)`)
	_, err = Open(t.Context(), path, testOptions())
	requireCode(t, err, publicerr.SchemaRefused)
}

func TestCallerCancellationBoundsWriterWait(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	held, err := s.writer.BeginTx(t.Context(), nil)
	requireOK(t, err)
	defer func() { _ = held.Rollback() }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.RegisterUpstream(ctx, stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func independentHead(t *testing.T, db *sql.DB) stateport.AuditHead {
	t.Helper()
	var head stateport.AuditHead
	requireOK(t, db.QueryRowContext(t.Context(), `SELECT id,hash FROM audit_head`).Scan(&head.ID, &head.SHA256))
	return head
}
