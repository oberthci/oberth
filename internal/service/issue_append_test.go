package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestIssueAppendConcatenatesWithSeparator(t *testing.T) {
	ctx := context.Background()
	fixture := newControlFixture(t)
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	issue, err := fixture.store.CreateManualIssue(ctx, actor.Identity, model.ManualIssueSpec{
		Title: "campaign issue",
		Body:  "original body",
	})
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(fmt.Sprintf(`{"id":%d,"text":"checkpoint 1"}`, issue.ID))
	value, err := service.CallTool(ctx, actor, "issue_append", args)
	if err != nil {
		t.Fatalf("issue_append: %v", err)
	}
	got, ok := value.(api.IssueResponse)
	if !ok {
		t.Fatalf("unexpected type %T", value)
	}
	if got.ID != issue.ID {
		t.Fatalf("id = %d, want %d", got.ID, issue.ID)
	}
	if !strings.HasPrefix(got.Body, "original body") {
		t.Fatalf("body does not start with original: %s", got.Body)
	}
	if !strings.Contains(got.Body, "---") {
		t.Fatalf("body missing separator: %s", got.Body)
	}
	if !strings.Contains(got.Body, "_Appended ") {
		t.Fatalf("body missing dated marker: %s", got.Body)
	}
	if !strings.HasSuffix(got.Body, "checkpoint 1") {
		t.Fatalf("body does not end with appended text: %s", got.Body)
	}
	// updated_at must advance
	if got.UpdatedAt == issue.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00") {
		t.Fatal("updated_at did not advance")
	}
	// Verify audit chain
	if _, err := fixture.store.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIssueAppendOnEmptyBody(t *testing.T) {
	ctx := context.Background()
	fixture := newControlFixture(t)
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	issue, err := fixture.store.CreateManualIssue(ctx, actor.Identity, model.ManualIssueSpec{
		Title: "empty body issue",
		Body:  "",
	})
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(fmt.Sprintf(`{"id":%d,"text":"first entry"}`, issue.ID))
	value, err := service.CallTool(ctx, actor, "issue_append", args)
	if err != nil {
		t.Fatalf("issue_append on empty body: %v", err)
	}
	got := value.(api.IssueResponse)
	if !strings.Contains(got.Body, "first entry") {
		t.Fatalf("body missing appended text: %s", got.Body)
	}
}

func TestIssueAppendRejectsEmptyText(t *testing.T) {
	ctx := context.Background()
	fixture := newControlFixture(t)
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	issue, err := fixture.store.CreateManualIssue(ctx, actor.Identity, model.ManualIssueSpec{
		Title: "reject empty", Body: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"", "  ", "\t\n"} {
		args := json.RawMessage(fmt.Sprintf(`{"id":%d,"text":%q}`, issue.ID, text))
		if _, err := service.CallTool(ctx, actor, "issue_append", args); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("append %q: %v, want ErrInvalidInput", text, err)
		}
	}
}

func TestIssueAppendRejectsNonExistentIssue(t *testing.T) {
	ctx := context.Background()
	fixture := newControlFixture(t)
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	args := json.RawMessage(`{"id":999999,"text":"content"}`)
	if _, err := service.CallTool(ctx, actor, "issue_append", args); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("append to absent issue: %v, want ErrNotFound", err)
	}
}

func TestIssueAppendRespectsLock(t *testing.T) {
	ctx := context.Background()
	fixture := newControlFixture(t)
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	issue, err := fixture.store.CreateManualIssue(ctx, actor.Identity, model.ManualIssueSpec{
		Title: "locked", Body: "original",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AcquireIssueLock(ctx, issue.ID, "other@host"); err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(fmt.Sprintf(`{"id":%d,"text":"should fail"}`, issue.ID))
	if _, err := service.CallTool(ctx, actor, "issue_append", args); !errors.Is(err, store.ErrLockHeld) {
		t.Fatalf("append to locked issue: %v, want ErrLockHeld", err)
	}
}

func TestIssueAppendRequiresMutationGate(t *testing.T) {
	fixture := newControlFixture(t)
	service := fixture.api(t)
	gateErr := errors.New("audit unavailable")
	service.mutationGate = func(context.Context) error { return gateErr }
	_, err := service.CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "issue_append", json.RawMessage(`{"id":1,"text":"x"}`))
	if !errors.Is(err, gateErr) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("append bypassed mutation gate: %v", err)
	}
}

func TestIssueUpdateExpectedBodySHA256AcceptsMatch(t *testing.T) {
	ctx := context.Background()
	fixture := newControlFixture(t)
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	body := "known body content"
	issue, err := fixture.store.CreateManualIssue(ctx, actor.Identity, model.ManualIssueSpec{
		Title: "sha256 test", Body: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(sum[:])
	args := json.RawMessage(fmt.Sprintf(`{"id":%d,"title":"sha256 test","body":"new body","expected_body_sha256":"%s"}`, issue.ID, hash))
	value, err := service.CallTool(ctx, actor, "issue_update", args)
	if err != nil {
		t.Fatalf("issue_update with matching sha256: %v", err)
	}
	got := value.(api.IssueResponse)
	if got.Body != "new body" {
		t.Fatalf("body = %q, want %q", got.Body, "new body")
	}
}

func TestIssueUpdateExpectedBodySHA256RejectsMismatch(t *testing.T) {
	ctx := context.Background()
	fixture := newControlFixture(t)
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	issue, err := fixture.store.CreateManualIssue(ctx, actor.Identity, model.ManualIssueSpec{
		Title: "sha256 mismatch", Body: "original body",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Send a wrong hash — simulates a truncated payload where the caller
	// thought the body was different.
	wrongHash := "0000000000000000000000000000000000000000000000000000000000000000"
	args := json.RawMessage(fmt.Sprintf(`{"id":%d,"title":"sha256 mismatch","body":"truncated replacement","expected_body_sha256":"%s"}`, issue.ID, wrongHash))
	_, err = service.CallTool(ctx, actor, "issue_update", args)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("issue_update with wrong sha256: %v, want ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "expected_body_sha256 mismatch") {
		t.Fatalf("error message = %q, want mention of sha256 mismatch", err.Error())
	}
	// Verify the body was NOT changed
	current, err := fixture.store.Issue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Body != "original body" {
		t.Fatalf("body was changed despite hash mismatch: %q", current.Body)
	}
}

func TestIssueUpdateWithoutSHA256StillWorks(t *testing.T) {
	ctx := context.Background()
	fixture := newControlFixture(t)
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	issue, err := fixture.store.CreateManualIssue(ctx, actor.Identity, model.ManualIssueSpec{
		Title: "no hash", Body: "original",
	})
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(fmt.Sprintf(`{"id":%d,"title":"no hash","body":"updated"}`, issue.ID))
	value, err := service.CallTool(ctx, actor, "issue_update", args)
	if err != nil {
		t.Fatalf("issue_update without sha256: %v", err)
	}
	got := value.(api.IssueResponse)
	if got.Body != "updated" {
		t.Fatalf("body = %q, want %q", got.Body, "updated")
	}
}
