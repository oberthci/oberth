package service

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
)

func TestLegacyPersistedAttemptsResolveDeclaredRows(t *testing.T) {
	for _, test := range []struct {
		name     string
		attempts []int
		statuses []model.StepStatus
	}{
		{"first-passes", []int{0}, []model.StepStatus{model.StepPassed}},
		{"fail-then-pass", []int{0, 1}, []model.StepStatus{model.StepFailed, model.StepPassed}},
		{"exhausted", []int{0, 1}, []model.StepStatus{model.StepFailed, model.StepFailed}},
		{"numeric-order", []int{2, 10}, []model.StepStatus{model.StepFailed, model.StepPassed}},
	} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", test.name, reverse), func(t *testing.T) {
				result := JobResult{Status: model.RunPassed, Phase: "passed"}
				var want []model.StepResult
				var ranges []runlog.Range
				for position, attempt := range test.attempts {
					burn := fmt.Sprintf("probe(%d)", attempt)
					// The latest attempt deliberately has older timestamps.
					at := time.Unix(1000-int64(position), 0).UTC()
					want = nil
					for index, name := range []string{"get", "check"} {
						step := model.StepResult{Burn: burn, Step: name, Status: model.StepPassed,
							StartedAt: &at, FinishedAt: &at}
						if name == "check" {
							step.Status = test.statuses[position]
							if step.Status == model.StepFailed {
								step.ExitCode = 42
							}
						}
						result.Steps = append(result.Steps, step)
						start := int64(100*(position+1) + 10*index)
						ranges = append(ranges, runlog.Range{Burn: burn, Step: name, Start: start, End: start + 10})
						step.Burn, step.Ordinal, step.LogStart, step.LogEnd = "probe", 25+index, start, start+10
						want = append(want, step)
					}
				}
				if want[1].Status == model.StepFailed {
					result.Status = model.RunFailed
					result.FailedBurn = fmt.Sprintf("probe(%d)", test.attempts[len(test.attempts)-1])
					result.FailedStep = "check"
				}
				if reverse {
					slices.Reverse(result.Steps)
				}
				fixture := newControlFixture(t, result)
				fixture.jobs.plan = []runprogress.PlannedStep{
					{Burn: "probe", Step: "get", Ordinal: 25},
					{Burn: "probe", Step: "check", Ordinal: 26},
				}
				ctx := context.Background()
				run, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "legacy", Repository: fixture.repo,
					Branch: "feature/legacy", SHA: strings.Repeat("b", 40), Actor: "agent@host"})
				if err != nil {
					t.Fatal(err)
				}
				if err := fixture.scheduler.ProcessNext(ctx); err != nil {
					t.Fatal(err)
				}
				if err := fixture.scheduler.persistSteps(ctx, run.ID, result.Steps, runlog.Index{Ranges: ranges}); err != nil {
					t.Fatal(err)
				}
				control := fixture.api(t)
				steps := runDetailSteps(t, control, run.ID)
				if len(steps) != len(want) {
					t.Fatalf("legacy duplicate or missing declaration: %+v", steps)
				}
				for index, step := range steps {
					expected := want[index]
					if step.Burn != expected.Burn || step.Step != expected.Step || step.Ordinal != expected.Ordinal ||
						step.Status != expected.Status || step.ExitCode != expected.ExitCode || step.LogStart != expected.LogStart ||
						step.LogEnd != expected.LogEnd || !reflect.DeepEqual(step.StartedAt, expected.StartedAt) ||
						!reflect.DeepEqual(step.FinishedAt, expected.FinishedAt) {
						t.Fatalf("legacy outcome: %+v, want %+v", step, expected)
					}
				}
				status, err := control.status(ctx, fixture.repo.Name, run.SHA, "")
				if err != nil || len(status.Burns) != 1 || status.Burns["probe"] != string(want[1].Status) {
					t.Fatalf("legacy status: %+v: %v", status, err)
				}
				if result.Status == model.RunFailed && (status.ExitCode == nil || *status.ExitCode != 42) {
					t.Fatalf("legacy failure lost exit code: %+v", status)
				}
			})
		}
	}
}

func TestLegacyAliasesPreserveLiteralNamesAndHooks(t *testing.T) {
	plan := []runprogress.PlannedStep{
		{Burn: "probe", Step: "check", Ordinal: 26},
		{Burn: "probe(3)", Step: "check", Ordinal: 30},
	}
	rows := []model.StepResult{
		{Burn: "probe(0)", Step: "check", Status: model.StepFailed, ExitCode: 42},
		{Burn: "probe(3)", Step: "check", Status: model.StepPassed},
		{Burn: "probe(99).onExit", Step: "check", Status: model.StepPassed},
		{Burn: "probe.onExit(99)", Step: "check", Status: model.StepPassed},
		{Burn: "unrelated(99)", Step: "check", Status: model.StepPassed},
	}
	steps := mergeSteps(model.Run{Status: model.RunFailed}, plan, nil, rows)
	if len(steps) != len(rows) {
		t.Fatalf("lost literal or hook rows: %+v", steps)
	}
	for _, step := range steps {
		if step.Burn == "probe" && (step.Status != model.StepFailed || step.Ordinal != 26 || step.ExitCode != 42) {
			t.Fatalf("hook or literal name replaced actual retry outcome: %+v", step)
		}
		if step.Burn == "probe(3)" && (step.Status != model.StepPassed || step.Ordinal != 30) {
			t.Fatalf("changed explicitly planned suffix: %+v", step)
		}
	}
	// A literal failed burn must still select its own exit code.
	burns, code := wireBurns([]model.StepResult{
		{Burn: "probe", Step: "check", Status: model.StepFailed, ExitCode: 1},
		{Burn: "probe(3)", Step: "check", Status: model.StepFailed, ExitCode: 42},
	}, model.Run{FailedBurn: "probe(3)", FailedStep: "check"})
	if burns["probe(3)"] != "failed" || code == nil || *code != 42 {
		t.Fatalf("literal failure identity changed: %v, %v", burns, code)
	}
}

func TestCanonicalOutcomeSupersedesLegacyAlias(t *testing.T) {
	plan := []runprogress.PlannedStep{{Burn: "probe", Step: "check", Ordinal: 26}}
	canonical := model.StepResult{Burn: "probe", Step: "check", Status: model.StepPassed, Ordinal: 26}
	legacy := model.StepResult{Burn: "probe(0)", Step: "check", Status: model.StepFailed, ExitCode: 42}
	for _, rows := range [][]model.StepResult{{legacy, canonical}, {canonical, legacy}} {
		steps := mergeSteps(model.Run{Status: model.RunPassed}, plan, nil, rows)
		if len(steps) != 1 || !reflect.DeepEqual(steps[0], canonical) {
			t.Fatalf("stale legacy failure replaced canonical outcome: %+v", steps)
		}
	}
}
