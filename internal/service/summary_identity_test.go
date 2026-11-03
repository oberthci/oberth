package service

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestSummaryIdentityMatchesDetail(t *testing.T) {
	f := newScopeFixture(t)
	for _, run := range f.runs["github"] {
		detail, err := f.call(scopedActor, "run_get", map[string]any{"id": run.ID})
		if err != nil {
			t.Fatal(err)
		}
		value, err := f.call(scopedActor, "run_get", map[string]any{"id": run.ID[:12], "summary": true})
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(value)
		var got map[string]any
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		shown := detail.(RunDetailResponse).Run
		for key, want := range map[string]string{"repo": "github/oberthci/oberth", "ref": run.Ref, "sha": run.SHA, "display_status": shown.DisplayStatus, "display_reason": shown.DisplayReason} {
			if got[key] != want {
				t.Errorf("%s=%v want %q", key, got[key], want)
			}
		}
	}
}

func TestPromotionResponseIdentity(t *testing.T) {
	f := newScopeFixture(t)
	p := f.promoted["github"]
	for _, terminal := range []bool{false, true} {
		if terminal {
			var err error
			p, err = f.store.FinishPromotion(t.Context(), p.ID, model.PromotionFailed, "", "failed candidate")
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, tool := range []string{"promote_status", "promotion_list"} {
			args := map[string]any{"id": p.ID[:12]}
			if tool == "promotion_list" {
				args = map[string]any{"repo": "github/oberthci/oberth"}
			}
			value, err := f.call(scopedActor, tool, args)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(value)
			var got map[string]any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if tool == "promotion_list" {
				got = got["promotions"].([]any)[0].(map[string]any)
			}
			if got["repo"] != "github/oberthci/oberth" || got["ref"] != p.SourceBranch {
				t.Fatalf("%s identity: %s", tool, encoded)
			}
		}
	}
}
