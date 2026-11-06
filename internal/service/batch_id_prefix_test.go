package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestBatchRunPriorityUniquePrefixes(t *testing.T) {
	f := newControlFixture(t)
	control, err := NewAPI(APIConfig{Runs: f.store, History: f.store, QueuePriorities: f.store, Enqueues: f.scheduler})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{8, 12, 32} {
		run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: fmt.Sprintf("feature/prefix-%d", n), SHA: strings.Repeat("a", 40), Actor: "dev", Trigger: "push"})
		if err != nil {
			t.Fatal(err)
		}
		value, err := control.CallTool(t.Context(), api.Actor{Identity: "admin", Admin: true}, "run_priority", json.RawMessage(fmt.Sprintf(`{"run_id":%q,"priority":"high","reason":"prioritize queued run"}`, run.ID[:n])))
		if err != nil {
			t.Errorf("%d characters: %v", n, err)
			continue
		}
		if got := value.(model.Run); got.ID != run.ID || got.Priority != model.PriorityHigh {
			t.Fatalf("wrong run: %+v", got)
		}
	}
}

func TestBatchPromotionToolsUniquePrefixes(t *testing.T) {
	f := newControlFixture(t)
	control := f.api(t)
	actor := api.Actor{Identity: "admin", Admin: true}
	p, err := f.store.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/prefix", SourceSHA: strings.Repeat("b", 40), TargetRef: "main", Actor: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"promote_status", "promote_retry", "publish_retry", "promotion_list"} {
		t.Run(tool, func(t *testing.T) {
			key := "id"
			if tool == "promotion_list" {
				key = "before"
			}
			call := func(id string) (string, error) {
				raw, _ := json.Marshal(map[string]string{key: id})
				value, err := control.CallTool(t.Context(), actor, tool, raw)
				body, _ := json.Marshal(value)
				return string(body), err
			}
			full, fullErr := call(p.ID)
			for _, n := range []int{8, 12} {
				got, err := call(p.ID[:n])
				if fmt.Sprint(err) != fmt.Sprint(fullErr) || got != full {
					t.Errorf("%d characters: got %s / %v; full ID %s / %v", n, got, err, full, fullErr)
				}
			}
		})
	}
}

func TestBatchPromotionPrefixesRespectScope(t *testing.T) {
	f := newScopeFixture(t)
	control := newControlFixture(t)
	f.service.git = control.git
	f.service.enqueues = control.scheduler
	f.service.promotionRuns = f.store
	for _, tool := range []string{"promote_status", "promote_retry", "promotion_list"} {
		t.Run(tool, func(t *testing.T) {
			key := "id"
			if tool == "promotion_list" {
				key = "before"
			}
			for _, n := range []int{8, 12, 32} {
				_, err := f.call(scopedActor, tool, map[string]string{key: f.promoted["codeberg"].ID[:n]})
				if !errors.Is(err, store.ErrNotFound) {
					t.Errorf("hidden %d: %v", n, err)
				}
				value, err := f.call(scopedActor, tool, map[string]string{key: f.promoted["github"].ID[:n]})
				if errors.Is(err, store.ErrNotFound) || errors.Is(err, ErrUnavailable) {
					t.Errorf("visible %d: %+v %v", n, value, err)
				}
			}
		})
	}
}

// Every public ID boundary must propagate ambiguity before it can mutate state.
type batchAmbiguousHistory struct{ RunHistory }

func (batchAmbiguousHistory) ResolveRunID(context.Context, string) (model.Run, error) {
	return model.Run{}, fmt.Errorf("%w: matches: %s, %s", store.ErrAmbiguous, batchMatchA, batchMatchB)
}

type batchAmbiguousPromotions struct{ PromotionRepository }

func (batchAmbiguousPromotions) ResolvePromotionID(context.Context, string) (model.Promotion, error) {
	return model.Promotion{}, fmt.Errorf("%w: matches: %s, %s", store.ErrAmbiguous, batchMatchA, batchMatchB)
}

const batchMatchA = "1234abcd5678aaaaaaaaaaaaaaaaaaaa"
const batchMatchB = "1234abcd5678bbbbbbbbbbbbbbbbbbbb"

func TestBatchToolAmbiguousPrefixes(t *testing.T) {
	f := newControlFixture(t)
	control := f.api(t)
	kept := newRetentionFixture(t)
	control.history = batchAmbiguousHistory{f.store}
	control.promotions = batchAmbiguousPromotions{f.store}
	control.runCancels, control.runReruns, control.runRetries = f.store, f.store, f.store
	control.queuePriorities, control.admission = f.store, f.scheduler
	control.artifacts = kept.retention.Artifacts
	for _, tool := range []string{"run_get", "run_logs", "run_cancel", "run_rerun", "run_retry", "run_resources", "run_priority", "admission_release", "artifacts", "artifact_get", "wait", "publish_retry", "run_list", "promote_status", "promote_retry", "promotion_list"} {
		for _, n := range []int{8, 12} {
			t.Run(fmt.Sprintf("%s/%d", tool, n), func(t *testing.T) {
				key := "id"
				args := map[string]any{}
				switch tool {
				case "run_priority":
					key = "run_id"
					args["priority"] = "high"
					args["reason"] = "release blocker"
				case "admission_release":
					key = "run_id"
				case "run_resources":
					key = "run"
				case "run_list":
					key = "before"
				case "promotion_list":
					key = "before"
				case "run_cancel", "run_rerun":
					args["reason"] = "retry queued work"
				case "artifact_get":
					args["name"] = "report.txt"
				}
				args[key] = batchMatchA[:n]
				raw, _ := json.Marshal(args)
				_, err := control.CallTool(t.Context(), api.Actor{Identity: "admin", Admin: true}, tool, raw)
				if !errors.Is(err, store.ErrAmbiguous) || !strings.Contains(err.Error(), batchMatchA) || !strings.Contains(err.Error(), batchMatchB) {
					t.Fatalf("ambiguity lost: %v", err)
				}
			})
		}
	}
}
