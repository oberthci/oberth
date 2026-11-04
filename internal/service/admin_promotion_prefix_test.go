package service

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestAdminPromotionPrefixesAcrossPublicSelectors(t *testing.T) {
	f := newControlFixture(t)
	c := f.api(t)
	p, err := f.store.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/prefix", SourceSHA: strings.Repeat("a", 40), TargetRef: "main", Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	actor := api.Actor{Identity: "operator", Admin: true}
	for _, tool := range []string{"promote_status", "promote_retry", "publish_retry", "promotion_list"} {
		key := "id"
		if tool == "promotion_list" {
			key = "before"
		}
		raw, _ := json.Marshal(map[string]any{key: p.ID[:8]})
		_, err = c.CallTool(t.Context(), actor, tool, raw)
		switch tool {
		case "promote_status", "promotion_list":
			if err != nil {
				t.Fatalf("%s prefix: %v", tool, err)
			}
		default:
			if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), p.ID) {
				t.Fatalf("%s did not resolve before state refusal: %v", tool, err)
			}
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(f.root, "oberth.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, id := range []string{"feedface" + strings.Repeat("a", 24), "feedface" + strings.Repeat("b", 24)} {
		if _, err = db.Exec(`INSERT INTO promotions(id,repo_id,source_branch,source_sha,target_ref,previous_sha,result_sha,actor,status,created_at,updated_at) SELECT ?,repo_id,source_branch,source_sha,target_ref,previous_sha,result_sha,actor,'pending',created_at,updated_at FROM promotions WHERE id=?`, id, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"promote_status", "promote_retry", "publish_retry", "promotion_list"} {
		key := "id"
		if tool == "promotion_list" {
			key = "before"
		}
		raw, _ := json.Marshal(map[string]any{key: "feedface"})
		if _, err = c.CallTool(t.Context(), actor, tool, raw); !errors.Is(err, store.ErrAmbiguous) {
			t.Fatalf("%s lost ambiguity: %v", tool, err)
		}
	}
}
