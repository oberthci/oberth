package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestRunGetPagesNodesAndSanitizesMessages(t *testing.T) {
	f := newControlFixture(t)
	run, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "node-summary", Repository: f.repo, Branch: "nodes", SHA: strings.Repeat("a", 40), Actor: "reader"})
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]model.StepResult, 205)
	for i := range rows {
		rows[i] = model.StepResult{NodeID: fmt.Sprintf("node-%03d", i), Ordinal: i, Status: model.StepFailed, Message: "\x1b[31m" + strings.Repeat("界", 600) + "\x1b[0m"}
	}
	rows[0].Status = model.StepPassed
	if err = f.store.PutStepResults(t.Context(), run.ID, rows); err != nil {
		t.Fatal(err)
	}
	service := f.api(t)
	offset, total := 0, 0
	for {
		value, err := service.CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_get", json.RawMessage(fmt.Sprintf(`{"id":%q,"step_offset":%d}`, run.ID, offset)))
		if err != nil {
			t.Fatal(err)
		}
		page := value.(RunSummaryResponse)
		if page.StepCount != 205 || len(page.Steps) > 100 || len(page.Steps) == 0 {
			t.Fatal(page.StepCount, len(page.Steps))
		}
		for i, step := range page.Steps {
			if step.NodeID != rows[offset+i].NodeID {
				t.Fatal("page lost native order", step.NodeID)
			}
			want := strings.Repeat("界", 512)
			if offset+i == 0 {
				want = ""
			}
			if step.Message != want {
				t.Fatal("unbounded or unsafe summary message", step.NodeID)
			}
		}
		total += len(page.Steps)
		if page.NextStepOffset == nil {
			break
		}
		offset = *page.NextStepOffset
	}
	if total != 205 {
		t.Fatal("paged nodes lost", total)
	}
	value, err := service.CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_get", json.RawMessage(fmt.Sprintf(`{"id":%q,"summary":false}`, run.ID)))
	if err != nil {
		t.Fatal(err)
	}
	full := value.(RunDetailResponse)
	if len(full.Steps) != 205 || full.Steps[0].Message != strings.Repeat("界", 512) {
		t.Fatal("full reader bypassed message projection")
	}
	for _, args := range []string{`"step_offset":-1`, `"step_limit":101`, `"step_limit":-1`} {
		if _, err := service.CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_get", json.RawMessage(fmt.Sprintf(`{"id":%q,%s}`, run.ID, args))); err == nil {
			t.Fatal("invalid paging accepted", args)
		}
	}
	value, err = service.CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_get", json.RawMessage(fmt.Sprintf(`{"id":%q,"step_offset":999,"step_limit":2}`, run.ID)))
	if err != nil || len(value.(RunSummaryResponse).Steps) != 0 {
		t.Fatal("out-of-range page", value, err)
	}
}
