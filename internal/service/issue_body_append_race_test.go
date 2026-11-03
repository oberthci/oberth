package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// Hold the replacement after service validation, before the real store call.
// An append can commit in this gap; only the transaction's hash check protects it.
type heldIssueBodyUpdate struct {
	IssueRepository
	entered chan struct{}
	release <-chan struct{}
}

func (s *heldIssueBodyUpdate) UpdateIssue(ctx context.Context, actor string, id int64, patch model.IssuePatch) (model.Issue, error) {
	close(s.entered)
	select {
	case <-s.release:
		return s.IssueRepository.UpdateIssue(ctx, actor, id, patch)
	case <-ctx.Done():
		return model.Issue{}, ctx.Err()
	}
}

func TestIssueBodyHashPreservesConcurrentAppend(t *testing.T) {
	f := newControlFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), testStartTimeout(10*time.Second))
	defer cancel()
	issue, err := f.store.CreateManualIssue(ctx, "writer", model.ManualIssueSpec{Title: "original title", Body: "original body"})
	if err != nil {
		t.Fatal(err)
	}
	service := f.api(t)
	release := make(chan struct{})
	held := &heldIssueBodyUpdate{IssueRepository: service.issues, entered: make(chan struct{}), release: release}
	service.issues = held
	sum := sha256.Sum256([]byte(issue.Body))
	args, err := json.Marshal(map[string]any{
		"id": issue.ID, "title": "stale title", "body": "stale replacement",
		"expected_body_sha256": hex.EncodeToString(sum[:]), "expected_revision": issue.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		_, err := service.CallTool(ctx, api.Actor{Identity: "writer"}, "issue_update", args)
		done <- err
	}()
	// Even an assertion failure must stop and join the blocked writer before
	// the fixture closes the database.
	defer func() { cancel(); <-joined }()
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("replacement did not reach the store boundary", ctx.Err())
	}
	appended, err := f.store.AppendIssueBody(ctx, "appender", issue.ID, "concurrent evidence must survive")
	if err != nil {
		t.Fatal(err)
	}
	if appended.Revision != issue.Revision {
		t.Fatal("body-only append unexpectedly changed the metadata revision")
	}
	before, err := f.store.VerifyAuditState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "expected_body_sha256 mismatch") {
			t.Fatalf("stale replacement after committed append: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("replacement did not finish", ctx.Err())
	}
	got, err := f.store.Issue(ctx, issue.ID)
	if err != nil || !reflect.DeepEqual(got, appended) {
		t.Fatalf("stale update changed committed issue: got=%+v want=%+v err=%v", got, appended, err)
	}
	after, err := f.store.VerifyAuditState(ctx)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("refused replacement changed audit: got=%+v want=%+v err=%v", after, before, err)
	}
}
