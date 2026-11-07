package gitcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// quarantinedDeletion leaves the v0.17.95 incident's shape: a ready deletion
// record whose state an operator edited to "quarantined".
func quarantinedDeletion(t *testing.T) (*testRepository, *Cache, string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cache")
	repository, cache, ready, featureSHA := deletionFixture(t, root)
	_ = cache.receiveTransaction(t.Context(), "example", ready.Path, "operator@host", func() error {
		return cache.deleteRef(t.Context(), ready.Path, "refs/heads/feature/gone", featureSHA)
	}, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return errors.New("process stopped") }))
	file, err := cache.reservationPath("example")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(body), `"state":"ready"`, `"state":"quarantined"`, 1)
	if edited == string(body) {
		t.Fatalf("record has no ready state: %s", body)
	}
	if err := os.WriteFile(file, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	return repository, newTestCacheAt(t, root, repository.upstream), file, featureSHA
}

func TestRetryRestoresOperatorQuarantinedRecord(t *testing.T) {
	t.Parallel()
	_, cache, file, featureSHA := quarantinedDeletion(t)
	record, err := cache.InspectReceiveRecord("example")
	if err != nil || !record.Corrupt || !record.Retryable || record.State != "quarantined" || len(record.Updates) != 1 || record.Updates[0].OldSHA != featureSHA {
		t.Fatalf("inspect = %+v, %v", record, err)
	}
	if _, err := cache.RetryReceiveRecord(t.Context(), "example", "wrong-id", ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return nil })); err == nil {
		t.Fatal("retry accepted another record's id")
	}
	var delivered []ReceiveEvent
	if _, err := cache.RetryReceiveRecord(t.Context(), "example", record.ID, ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		delivered = append(delivered, event)
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 || delivered[0].Actor != "operator@host" || !delivered[0].Update.Deleted() {
		t.Fatalf("delivered = %+v", delivered)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record kept after delivery: %v", err)
	}
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatalf("repository still frozen: %v", err)
	}
}

func TestAbandonMovesRecordAsideAndUnfreezesRepository(t *testing.T) {
	t.Parallel()
	_, cache, file, featureSHA := quarantinedDeletion(t)
	// Break the record beyond the quarantine marker: retry must refuse.
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(strings.Replace(string(body), `"version":2`, `"version":9`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	record, err := cache.InspectReceiveRecord("example")
	if err != nil || record.Retryable {
		t.Fatalf("inspect = %+v, %v", record, err)
	}
	if _, err := cache.RetryReceiveRecord(t.Context(), "example", record.ID, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return nil })); err == nil {
		t.Fatal("retry delivered a corrupt record")
	}
	if _, err := cache.PruneBranches(t.Context(), "example", []BranchHead{{Branch: "feature/gone", SHA: featureSHA}}, false); err == nil {
		t.Fatal("repository not frozen before abandon")
	}
	abandoned, err := cache.AbandonReceiveRecord(t.Context(), "example", record.ID)
	if err != nil || !strings.HasPrefix(abandoned.MovedTo, "abandoned/") {
		t.Fatalf("abandon = %+v, %v", abandoned, err)
	}
	if _, err := os.Stat(filepath.Join(cache.outboxRoot, abandoned.MovedTo)); err != nil {
		t.Fatalf("abandoned evidence missing: %v", err)
	}
	if pending, err := cache.PendingReceives(); err != nil || len(pending) != 0 {
		t.Fatalf("pending after abandon = %+v, %v", pending, err)
	}
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatalf("repository still frozen: %v", err)
	}
}
