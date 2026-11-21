package service_test

import (
	"encoding/json"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestMovingListsScopeBoundsAndPrivateAttribution(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	out := promote(t, f)
	call, err := f.s.PreparePromotionList(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromotionListArgs{Repo: repository, SourceSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	page, err := call(t.Context())
	if err != nil || len(page.Promotions) != 1 || page.Promotions[0].ID != out.ID {
		t.Fatal("promotion list lost canonical source", page, err)
	}
	checkPublicPromotionFields(t, page)
	_, err = f.s.PreparePublicationList(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PublicationListArgs{Repo: "hidden/team/repo"})
	requireCode(t, err, publicerr.NotFound)
	_, err = f.s.PreparePublicationAbandon(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PublicationAbandonArgs{ID: "publication:" + out.ID, Reason: "stop"})
	requireCode(t, err, publicerr.Forbidden)
}
func containsJSONField(data []byte, key string) bool {
	var object any
	if json.Unmarshal(data, &object) != nil {
		return false
	}
	return hasJSONField(object, key)
}
func hasJSONField(value any, key string) bool {
	switch v := value.(type) {
	case map[string]any:
		if _, ok := v[key]; ok {
			return true
		}
		for _, nested := range v {
			if hasJSONField(nested, key) {
				return true
			}
		}
	case []any:
		for _, nested := range v {
			if hasJSONField(nested, key) {
				return true
			}
		}
	}
	return false
}
func TestPromotionStatusTimeoutAndCompletion(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		f.seed(stepmodel.RunPassed)
		out := promote(t, f)
		p, _ := f.store.Promotion(t.Context(), out.ID)
		if err := f.store.PreparePromotion(t.Context(), stateport.Action{Actor: caller().Identity}, model.Promotion{ID: "pending", Repo: repository, ResultSHA: head, Actor: caller().Identity, Caller: caller(), Status: model.PromotionPending}); err != nil {
			t.Fatal(err)
		}
		call, err := f.s.PreparePromoteStatus(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteStatusArgs{ID: "pending", TimeoutS: 1})
		if err != nil {
			t.Fatal(err)
		}
		result, err := call(t.Context())
		if err != nil || !result.TimedOut || result.Promotion == nil || result.Promotion.Status != model.PromotionPending {
			t.Fatal("promotion timeout lied", result, err, p)
		}
	})
}

func checkPublicPromotionFields(t *testing.T, page apicontract.PromotionPage) {
	t.Helper()
	body, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"fingerprint", "grant_id", "sources", "source_count", "prune_source", "tag_after"} {
		if containsJSONField(body, private) {
			t.Fatal("private or retired promotion value exposed", private)
		}
	}
}
