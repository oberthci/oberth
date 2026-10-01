package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestPromotionListRecoversLostIDWithoutMutation(t *testing.T) {
	fixture := newControlFixture(t)
	service := fixture.api(t)
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	pending, err := fixture.store.AppendPromotion(ctx, model.PromotionSpec{RepoID: fixture.repo.ID, SourceBranch: "feature/lost-reply", SourceSHA: sha, TargetRef: "main", Actor: "original-actor"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := fixture.store.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gateCalls := 0
	service.mutationGate = func(context.Context) error { gateCalls++; return errors.New("mutation gate unavailable") }
	result, err := service.CallTool(ctx, api.Actor{Identity: "read-only-agent"}, "promotion_list", json.RawMessage(fmt.Sprintf(`{"repo":"oberth","source_sha":%q,"status":"pending","limit":1}`, sha)))
	if err != nil {
		t.Fatal(err)
	}
	page, ok := result.(api.PromotionListResponse)
	if !ok || len(page.Promotions) != 1 || page.Promotions[0].ID != pending.ID || page.Promotions[0].Status != "pending" || page.Promotions[0].ResultSHA != "" || page.Promotions[0].RunID != "" {
		t.Fatalf("recovered admission = %#v", result)
	}
	body, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "original-actor") || strings.Contains(string(body), `"error"`) {
		t.Fatalf("internal fields leaked: %s", body)
	}
	after, err := fixture.store.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || gateCalls != 0 || len(fixture.git.promotions) != 0 {
		t.Fatal("read-only recovery mutated audit, gate or Git")
	}
	for _, raw := range []string{`{"source_sha":"aaaaaaa"}`, `{"source_sha":"main"}`, `{"status":"running"}`, `{"before":-1}`, `{"limit":201}`, `{"limit":-1}`, `{"unknown":true}`} {
		if _, err := service.CallTool(ctx, api.Actor{Identity: "read-only-agent"}, "promotion_list", json.RawMessage(raw)); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid %s returned %v", raw, err)
		}
	}
	if _, err := service.CallTool(ctx, api.Actor{Identity: "read-only-agent"}, "promotion_list", json.RawMessage(`{"repo":"absent"}`)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown repository: %v", err)
	}
}
