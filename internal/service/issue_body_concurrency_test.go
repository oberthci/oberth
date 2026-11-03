package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// Both writers reach the store after any service-level snapshot check, so the
// regression deterministically catches a check outside the write transaction.
type simultaneousIssueWriters struct {
	IssueRepository
	writers sync.WaitGroup
}

func (s *simultaneousIssueWriters) UpdateIssue(ctx context.Context, actor string, id int64, patch model.IssuePatch) (model.Issue, error) {
	s.writers.Done()
	s.writers.Wait()
	return s.IssueRepository.UpdateIssue(ctx, actor, id, patch)
}
func TestIssueBodyHashSerializesConcurrentWriters(t *testing.T) {
	f := newControlFixture(t)
	issue, err := f.store.CreateManualIssue(t.Context(), "writer", model.ManualIssueSpec{Title: "initial", Body: "original"})
	if err != nil {
		t.Fatal(err)
	}
	service := f.api(t)
	simultaneous := &simultaneousIssueWriters{IssueRepository: service.issues}
	simultaneous.writers.Add(2)
	service.issues = simultaneous
	sum := sha256.Sum256([]byte(issue.Body))
	done := make(chan error, 2)
	for i := range 2 {
		go func(i int) {
			args, _ := json.Marshal(map[string]any{"id": issue.ID, "body": fmt.Sprintf("replacement-%d", i), "expected_body_sha256": hex.EncodeToString(sum[:])})
			_, err := service.CallTool(t.Context(), api.Actor{Identity: "writer"}, "issue_update", args)
			done <- err
		}(i)
	}
	success, conflict := 0, 0
	for range 2 {
		err := <-done
		if err == nil {
			success++
		} else if errors.Is(err, ErrInvalidInput) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("same-hash writers: success=%d conflict=%d", success, conflict)
	}
}
