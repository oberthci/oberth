package service

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/store"
)

// Every run selector is id: run_id is an unknown argument.
func TestRunSelectorIsIDOnly(t *testing.T) {
	f := newScopeFixture(t)
	run := f.runs["github"][0]
	value, err := f.call(scopedActor, "run_get", map[string]any{"id": run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if detail := value.(RunDetailResponse); detail.Run.ID != run.ID || detail.Run.QueueSequence != 0 {
		t.Fatalf("lost identity or scope redaction: %+v", detail.Run)
	}
	for _, args := range []map[string]any{{"run_id": run.ID}, {"id": run.ID, "run_id": run.ID}} {
		if _, err := f.call(scopedActor, "run_get", args); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("run_id accepted: %v", err)
		}
	}
	if _, err := f.call(scopedActor, "run_get", map[string]any{"id": f.runs["codeberg"][0].ID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("hidden run escaped scope: %v", err)
	}
}
