package main

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestDecidingPromotionRequiresResultCommitAndRun(t *testing.T) {
	for _, missing := range []string{"result-commit", "run"} {
		t.Run(missing, func(t *testing.T) {
			sha := strings.Repeat("a", 40)
			promotion := &model.Promotion{ID: "promotion", Status: model.PromotionPassed, ResultSHA: sha}
			run := &stepmodel.RunSummary{ID: "deadbeef", Status: stepmodel.RunPassed}
			if missing == "result-commit" {
				promotion.ResultSHA = ""
			} else {
				run = nil
			}
			b := fake.Bindings()
			b.Promote.Handler = &fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult]{Value: apicontract.PromoteResult{ID: "promotion", TestedRunID: "tested"}}
			b.PromoteStatus.Handler = &fake.Handler[apicontract.PromoteStatusArgs, apicontract.PromotionWaitResult]{Value: apicontract.PromotionWaitResult{Promotion: promotion, Publication: &model.Publication{ID: "publication:promotion", Status: model.PublicationDelivered}}}
			waited := &fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{Run: run}}
			b.Wait.Handler = waited
			app, out, diagnostic := fixture(t, b)
			app.git = func(context.Context, []string) (string, error) { return sha, nil }
			if code := app.run(t.Context(), []string{"promote", "--repo", "source/team/widget"}); code != 1 {
				t.Fatal("incomplete promotion succeeded", code)
			}
			want := "A required service protocol is unavailable.\nUse a server and CLI build with the required operation contract.\n"
			if !strings.HasSuffix(diagnostic.String(), want) {
				t.Fatalf("protocol refusal=%q want suffix=%q", diagnostic.String(), want)
			}
			if strings.Contains(out.String(), "next: oberth release") {
				t.Fatal("incomplete evidence suggested release")
			}
			_, _, calls := waited.Observed()
			if missing == "result-commit" && calls != 0 {
				t.Fatal("waited without a result commit")
			}
		})
	}
}

func TestDecidingPromotionDefaultsToCheckoutHead(t *testing.T) {
	sha := strings.Repeat("a", 40)
	b := fake.Bindings()
	promoted := &fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult]{Value: apicontract.PromoteResult{ID: "promotion", TestedRunID: "tested"}}
	b.Promote.Handler = promoted
	b.PromoteStatus.Handler = &fake.Handler[apicontract.PromoteStatusArgs, apicontract.PromotionWaitResult]{Value: apicontract.PromotionWaitResult{Promotion: &model.Promotion{ID: "promotion", Status: model.PromotionPassed, ResultSHA: sha}, Publication: &model.Publication{ID: "publication:promotion", Status: model.PublicationDelivered}}}
	b.Wait.Handler = &fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "deadbeef", Status: stepmodel.RunPassed}}}
	app, _, diagnostic := fixture(t, b)
	app.git = func(_ context.Context, args []string) (string, error) {
		if args[len(args)-1] == "HEAD^{commit}" {
			return sha, nil
		}
		return strings.Repeat("b", 40), nil
	}
	if code := app.run(t.Context(), []string{"promote", "--repo", "source/team/widget"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	inputs, _, calls := promoted.Observed()
	if calls != 1 || inputs[0].SHA != sha {
		t.Fatal("promoted a branch name instead of checkout HEAD", inputs)
	}
}

func TestDecidingLogDrainsAdvancingPages(t *testing.T) {
	b := fake.Bindings()
	logs := &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{}
	calls := 0
	logs.Run = func(context.Context) (apicontract.LogResult, error) {
		inputs, _, _ := logs.Observed()
		request := inputs[len(inputs)-1]
		if request.Step != "build" {
			t.Fatal("log page lost selected step", request.Step)
		}
		offsets := []int64{0, 6, 11}
		if calls >= len(offsets) {
			t.Fatal("unexpected extra log page")
		}
		if calls == 0 && request.ByteOffset != nil || calls > 0 && (request.ByteOffset == nil || *request.ByteOffset != offsets[calls]) {
			t.Fatal("wrong retained-byte offset", request.ByteOffset, calls)
		}
		content := []string{"alpha\n", "beta\n", "gamma\n"}[calls]
		calls++
		return apicontract.LogResult{Content: content, Meta: logcontract.Meta{HasMore: calls < 3, NextByteOffset: offsets[calls-1] + int64(len(content))}}, nil
	}
	b.RunLogs.Handler = logs
	step := stepmodel.RunStep{Path: "build", Name: "build", Status: stepmodel.StepPassed}
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "deadbeef", Status: stepmodel.RunPassed, Steps: []stepmodel.RunStep{step}}}}
	app, out, diagnostic := fixture(t, b)
	if code := app.run(t.Context(), []string{"log", "deadbeef", "build"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	want := "== build ==\nalpha\nbeta\ngamma\nrun: deadbeef\nresult: passed\nnext:\noberth run get deadbeef\n"
	if out.String() != want || calls != 3 {
		t.Fatalf("log drain=%q calls=%d want=%q", out.String(), calls, want)
	}
}
