package service

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/store"
)

func TestRunIDCompatibilityAliasesUseScopedResolution(t *testing.T) {
	f := newScopeFixture(t)
	run := f.runs["github"][0]
	for _, args := range []map[string]any{{"id": run.ID}, {"run_id": run.ID}, {"id": run.ID, "run_id": run.ID}} {
		value, err := f.call(scopedActor, "run_get", args)
		if err != nil {
			t.Fatal(err)
		}
		detail := value.(RunDetailResponse)
		if detail.Run.ID != run.ID || detail.Run.QueueSequence != 0 {
			t.Fatalf("alias lost identity or scope redaction: %+v", detail.Run)
		}
	}
	for _, args := range []map[string]any{{"id": run.ID, "run_id": f.runs["codeberg"][0].ID}, {"run_id": nil}, {"run_id": 17}} {
		if _, err := f.call(scopedActor, "run_get", args); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("bad alias accepted: %v", err)
		}
	}
	if _, err := f.call(scopedActor, "run_get", map[string]any{"run_id": f.runs["codeberg"][0].ID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("hidden run escaped alias scope: %v", err)
	}
}

func TestRunIDAliasNormalizationPreservesOtherArguments(t *testing.T) {
	for _, tc := range []struct{ tool, raw, key string }{
		{"run_resources", `{"run_id":"aaaaaaaa","last":9007199254740993}`, "run"},
		{"wait", `{"run_id":"aaaaaaaa","timeout":1}`, "id"},
		{"run_get", `{"id":"aaaaaaaa","run_id":"aaaaaaaa"}`, "id"},
	} {
		raw, err := normalizeRunIDArgument(tc.tool, json.RawMessage(tc.raw))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields[tc.key]) != `"aaaaaaaa"` || fields["run_id"] != nil {
			t.Fatalf("%s: %s", tc.tool, raw)
		}
		if tc.tool == "run_resources" && string(fields["last"]) != "9007199254740993" {
			t.Fatal("integer lost precision", string(raw))
		}
	}
}
