package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestIssueBodyHashSerializesIndependentStoreConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "issues.sqlite")
	first, err := OpenAdminClient(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := OpenAdminClient(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	issue, err := first.CreateManualIssue(t.Context(), "writer", model.ManualIssueSpec{Title: "initial", Body: "original"})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(issue.Body))
	start := make(chan struct{})
	type result struct {
		issue model.Issue
		err   error
	}
	done := make(chan result, 2)
	for i, database := range []*Store{first, second} {
		go func() {
			<-start
			body := []string{"first replacement", "second replacement"}[i]
			got, err := database.UpdateIssue(t.Context(), "writer", issue.ID, model.IssuePatch{Body: &body, ExpectedBodySHA256: hex.EncodeToString(sum[:])})
			done <- result{got, err}
		}()
	}
	close(start)
	results := []result{<-done, <-done} // Join both before any fatal assertion.
	var winner model.Issue
	successes, conflicts := 0, 0
	for _, result := range results {
		switch {
		case result.err == nil:
			winner = result.issue
			successes++
		case errors.Is(result.err, ErrInvalid) && strings.Contains(result.err.Error(), "expected_body_sha256 mismatch"):
			conflicts++
		default:
			t.Fatal(result.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("same-hash writers on independent connections: successes=%d conflicts=%d", successes, conflicts)
	}
	got, err := first.Issue(t.Context(), issue.ID)
	if err != nil || !reflect.DeepEqual(got, winner) {
		t.Fatalf("winner not preserved: got=%+v want=%+v err=%v", got, winner, err)
	}
}

func TestIssueBodyHashMismatchRollsBackLockRenewalAndAudit(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	issue, err := s.CreateManualIssue(t.Context(), "writer", model.ManualIssueSpec{Title: "initial", Body: "original"})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := s.AcquireIssueLock(t.Context(), issue.ID, "writer")
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.VerifyAuditState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	body, title := "stale replacement", "stale title"
	_, err = s.UpdateIssue(t.Context(), "writer", issue.ID, model.IssuePatch{Body: &body, Title: &title, ExpectedBodySHA256: strings.Repeat("0", 64)})
	if !errors.Is(err, ErrInvalid) {
		t.Fatal("stale hash accepted", err)
	}
	got, err := s.Issue(t.Context(), issue.ID)
	if err != nil || !reflect.DeepEqual(got, issue) {
		t.Fatalf("stale update changed issue: %+v %v", got, err)
	}
	var expiry int64
	if err := s.db.QueryRowContext(t.Context(), `SELECT expires_at FROM issue_locks WHERE issue_id=?`, issue.ID).Scan(&expiry); err != nil || expiry != unixNano(lock.ExpiresAt) {
		t.Fatalf("conflict renewed lock: expiry=%d original=%d err=%v", expiry, unixNano(lock.ExpiresAt), err)
	}
	after, err := s.VerifyAuditState(t.Context())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("conflict changed audit: before=%+v after=%+v err=%v", before, after, err)
	}
}
