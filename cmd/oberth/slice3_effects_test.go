package main

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckoutPinsHostAndStrictKeys(t *testing.T) {
	scenario := newJoinScenario(t, "install")
	s := flowSession{app: scenario.app, ctx: t.Context()}
	if err := s.connect(); err != nil {
		t.Fatal(err)
	}
	defer s.close()
	known := filepath.Join(scenario.app.base, "install", "known_hosts")
	connection, command, err := s.checkoutConnection(known)
	dir := filepath.Join(scenario.app.base, "install")
	config, readErr := client.ReadPrivate(filepath.Join(dir, "ssh.config"))
	if err != nil || readErr != nil || !strings.HasPrefix(command, shellArgument(filepath.Join(dir, "git-ssh"))+" --config ") || connection.SSHURL != "ssh://git@oberth.local:2222" || !strings.Contains(string(config), "Port 2222\n") || !strings.Contains(string(config), "StrictHostKeyChecking yes") {
		t.Fatalf("unsafe SSH command: %s %v", command, err)
	}
	path := filepath.Join(scenario.app.base, "install", "installation.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "ssh://git@oberth.local", "ssh://git@foreign.local"))
	if err = client.WritePrivate(path, raw); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.checkoutConnection(known); err == nil {
		t.Fatal("foreign SSH host accepted")
	}
}
func TestJoinRecordsExactUplinkBinding(t *testing.T) {
	scenario := newJoinScenario(t, "install")
	original := scenario.app.git
	bound := ""
	scenario.app.git = func(ctx context.Context, args []string) (string, error) {
		if len(args) == 3 && args[0] == "config" && args[1] == "oberth.uplinkProfile" {
			bound = args[2]
		}
		return original(ctx, args)
	}
	if scenario.app.run(t.Context(), []string{"join", scenario.invitation}) != 0 || bound != scenario.uplink {
		t.Fatalf("wrong uplink binding: %q %s", bound, scenario.diagnostic.String())
	}
}
func TestPromotionFailurePendingAndFailedCI(t *testing.T) {
	for _, kind := range []string{"failed", "pending", "ci-failed"} {
		t.Run(kind, func(t *testing.T) { promotionRefusalCase(t, kind) })
	}
}
func promotionRefusalCase(t *testing.T, kind string) {
	t.Helper()
	b := fake.Bindings()
	sha := strings.Repeat("a", 40)
	b.Promote.Handler = &fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult]{Value: apicontract.PromoteResult{ID: "promotion", TestedRunID: "tested"}}
	h := &fake.Handler[apicontract.PromoteStatusArgs, apicontract.PromotionWaitResult]{}
	calls := 0
	h.Run = func(context.Context) (apicontract.PromotionWaitResult, error) {
		calls++
		status := model.PromotionPassed
		if kind == "failed" {
			status = model.PromotionFailed
		}
		if kind == "pending" {
			status = model.PromotionPending
			if calls == 2 {
				status = model.PromotionFailed
			}
		}
		return apicontract.PromotionWaitResult{Promotion: &model.Promotion{ID: "promotion", Status: status, ResultSHA: sha, Reason: "conflicts with main"}, Publication: &model.Publication{ID: "publication:promotion", Status: model.PublicationDelivered}}, nil
	}
	b.PromoteStatus.Handler = h
	run := &fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "failed-ci", Status: stepmodel.RunFailed}}}
	b.Wait.Handler = run
	app, out, diagnostic := fixture(t, b)
	app.git = func(context.Context, []string) (string, error) { return sha, nil }
	if code := app.run(t.Context(), []string{"promote", sha, "--repo", "project"}); code != 1 || strings.Contains(out.String(), "next:\noberth release") {
		t.Fatalf("promotion failure accepted: %d %s %s", code, out.String(), diagnostic.String())
	}
	if kind == "pending" && calls != 2 {
		t.Fatal("pending promotion treated as final")
	}
	_, _, waits := run.Observed()
	if kind == "ci-failed" {
		if !strings.Contains(out.String(), "oberth log failed-ci") {
			t.Fatal("missing failed CI remedy")
		}
		return
	}
	if waits != 0 {
		t.Fatal("failed promotion waited for CI")
	}
	if !strings.Contains(diagnostic.String(), "conflicts with main") {
		t.Fatal("promotion failure reason was lost", diagnostic.String())
	}
}
func TestReleaseFailsOnPublicationFailureAndGap(t *testing.T) {
	for _, kind := range []string{"failed", "gap"} {
		t.Run(kind, func(t *testing.T) {
			f := newReleaseFixture(t)
			events := []model.AgentEvent{{Kind: string(model.EventReleasePublished), RunID: "release"}}
			if kind == "failed" {
				events = append([]model.AgentEvent{{Kind: string(model.EventPublicationFailed), RunID: "release"}}, events...)
			}
			f.events.Value = apicontract.EventResult{EventPage: model.EventPage{Gap: kind == "gap", Events: events}}
			if f.run(t) != 1 {
				t.Fatal("publication failure or event gap accepted")
			}
		})
	}
}
func TestLogRefusesStalledCursor(t *testing.T) {
	b := fake.Bindings()
	calls := 0
	b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Run: func(context.Context) (apicontract.LogResult, error) {
		calls++
		if calls > 2 {
			return apicontract.LogResult{}, publicerr.New(publicerr.Unavailable, "Unexpected extra log request.", "Stop the stalled log cursor.", nil)
		}
		return apicontract.LogResult{Meta: logcontract.Meta{HasMore: true, NextByteOffset: 5}}, nil
	}}
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, stepmodel.RunSummary]{Value: stepmodel.RunSummary{ID: "run1", Status: stepmodel.RunRunning, Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepRunning}}}}
	app, _, diagnostic := fixture(t, b)
	if app.run(t.Context(), []string{"log", "run1"}) != 1 || !strings.Contains(diagnostic.String(), "cursor did not advance") {
		t.Fatal("stalled cursor accepted", diagnostic.String())
	}
}
func TestPromoteResolvesCommitLocally(t *testing.T) {
	app, _, diag := fixture(t, fake.Bindings())
	app.git = func(_ context.Context, args []string) (string, error) {
		if strings.Join(args, " ") != "rev-parse --verify --end-of-options typo^{commit}" {
			t.Fatal("unsafe or missing commit resolution", args)
		}
		return "", errors.New("missing commit")
	}
	if app.run(t.Context(), []string{"promote", "typo"}) != 2 || !strings.Contains(diag.String(), "typo") {
		t.Fatal("bad commit reached server", diag.String())
	}
}
