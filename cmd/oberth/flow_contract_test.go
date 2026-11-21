package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func auditRecord(id int64) apicontract.AuditRecord {
	return apicontract.AuditRecord{AuditAction: model.AuditAction{ID: id, Action: "changed", ResourceID: "project"}}
}

// Covers: REQ-C11-001
func TestCompleteAuditTailContract(t *testing.T) {
	b := fake.Bindings()
	h := &fake.Handler[apicontract.AuditListArgs, apicontract.AuditPage]{}
	calls := 0
	h.Run = func(context.Context) (apicontract.AuditPage, error) {
		calls++
		inputs, _, _ := h.Observed()
		before := inputs[len(inputs)-1].Before
		if before == 2 {
			return apicontract.AuditPage{Records: []apicontract.AuditRecord{auditRecord(1)}}, nil
		}
		if calls == 1 {
			return apicontract.AuditPage{Records: []apicontract.AuditRecord{auditRecord(3), auditRecord(2)}, NextCursor: "2"}, nil
		}
		return apicontract.AuditPage{Records: []apicontract.AuditRecord{auditRecord(4), auditRecord(3)}, NextCursor: "3"}, nil
	}
	b.AuditList.Handler = h
	app, out, diagnostic := fixture(t, b)
	if code := app.run(context.Background(), []string{"audit"}); code != 0 {
		t.Fatalf("audit failed %d: %s", code, diagnostic.String())
	}
	if out.String() != "1 changed project\n2 changed project\n3 changed project\n4 changed project\n" || calls != 4 {
		t.Fatalf("tail skipped/repeated: %q calls %d", out.String(), calls)
	}
}

func TestAuditNonAdvancingCursorStops(t *testing.T) {
	b := fake.Bindings()
	h := &fake.Handler[apicontract.AuditListArgs, apicontract.AuditPage]{Value: apicontract.AuditPage{Records: []apicontract.AuditRecord{auditRecord(3)}, NextCursor: "2"}}
	b.AuditList.Handler = h
	app, _, diagnostic := fixture(t, b)
	if code := app.run(context.Background(), []string{"audit"}); code != 1 || !strings.Contains(diagnostic.String(), "cursor did not advance") {
		t.Fatal("cursor did not stop", code, diagnostic.String())
	}
	_, _, calls := h.Observed()
	if calls != 2 {
		t.Fatal("cursor loop", calls)
	}
}

func TestPromoteReportsRunMainAndCI(t *testing.T) {
	b := fake.Bindings()
	mainSHA := strings.Repeat("a", 40)
	b.Promote.Handler = &fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult]{Value: apicontract.PromoteResult{ID: "promotion", TestedRunID: "tested-run"}}
	b.PromoteStatus.Handler = &fake.Handler[apicontract.PromoteStatusArgs, apicontract.PromotionWaitResult]{Value: apicontract.PromotionWaitResult{Promotion: &model.Promotion{ID: "promotion", Status: model.PromotionPassed, ResultSHA: mainSHA}, Publication: &model.Publication{ID: "publication:promotion", Status: model.PublicationDelivered}}}
	wait := &fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "ci-run", Status: stepmodel.RunPassed}}}
	b.Wait.Handler = wait
	app, out, diagnostic := fixture(t, b)
	app.git = func(context.Context, []string) (string, error) { return strings.Repeat("b", 40), nil }
	if code := app.run(context.Background(), []string{"promote", strings.Repeat("b", 40), "--repo", "project"}); code != 0 {
		t.Fatalf("promote %d: %s", code, diagnostic.String())
	}
	for _, fact := range []string{"tested-run", mainSHA, "ci-run", "passed", "next:"} {
		if !strings.Contains(out.String(), fact) {
			t.Fatal("missing promotion fact", fact)
		}
	}
	input, _, _ := wait.Observed()
	if len(input) != 1 || input[0].SHA != mainSHA {
		t.Fatal("waited on the source instead of resulting main")
	}
}

// A passed promotion whose main is not recorded as published is not a success;
// a timed-out status wait is observed again, and the person is told once what
// it waits for and for how long at most.
func TestPromoteRequiresItsDeliveredPublication(t *testing.T) {
	b := fake.Bindings()
	mainSHA := strings.Repeat("a", 40)
	b.Promote.Handler = &fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult]{Value: apicontract.PromoteResult{ID: "promotion", TestedRunID: "tested-run"}}
	passed := &model.Promotion{ID: "promotion", Status: model.PromotionPassed, ResultSHA: mainSHA}
	status := &fake.Handler[apicontract.PromoteStatusArgs, apicontract.PromotionWaitResult]{}
	status.Run = func(context.Context) (apicontract.PromotionWaitResult, error) {
		if _, _, calls := status.Observed(); calls < 3 {
			return apicontract.PromotionWaitResult{Promotion: passed, Publication: &model.Publication{ID: "publication:promotion", Status: model.PublicationPending, Reason: "waiting for a pending receive"}, TimedOut: true}, nil
		}
		return apicontract.PromotionWaitResult{Promotion: passed, Publication: &model.Publication{ID: "publication:promotion", Status: model.PublicationFailed, Reason: "waiting for a pending receive", Detail: "a receive is awaiting delivery"}}, nil
	}
	b.PromoteStatus.Handler = status
	app, out, diagnostic := fixture(t, b)
	app.git = func(context.Context, []string) (string, error) { return strings.Repeat("b", 40), nil }
	if code := app.run(context.Background(), []string{"promote", strings.Repeat("b", 40), "--repo", "project", "--detail"}); code != 1 {
		t.Fatalf("promote %d: %s", code, out.String())
	}
	for _, want := range []string{"The promoted commit is not recorded as published.", "publication:promotion failed: waiting for a pending receive", "a receive is awaiting delivery"} {
		if !strings.Contains(diagnostic.String(), want) {
			t.Fatal("missing publication outcome", want, diagnostic.String())
		}
	}
	waiting := "promotion passed; publishing: at most 5 attempts in about 30 minutes\npublication: waiting for a pending receive\n"
	if strings.Count(out.String(), waiting) != 1 || strings.Contains(diagnostic.String(), "did not reach the forge") {
		t.Fatal("the wait for the publication is not said once", out.String())
	}
	if _, _, calls := status.Observed(); calls != 3 || strings.Contains(out.String(), "next:") {
		t.Fatal("promotion reported before its publication settled", calls, out.String())
	}
}

// A promotion the server retests on a moved default branch ends with the
// retest's outcome; a retest still running when the wait times out is named
// with its state command, and promoting again is not asked for.
func TestPromoteFollowsTheRetestOnAMovedBranch(t *testing.T) {
	mainSHA := strings.Repeat("a", 40)
	for name, tc := range map[string]struct {
		status apicontract.PromotionWaitResult
		code   int
		want   []string
	}{
		"retest passed": {apicontract.PromotionWaitResult{Promotion: &model.Promotion{ID: "retest", Status: model.PromotionPassed, ResultSHA: mainSHA}, Publication: &model.Publication{ID: "publication:retest", Status: model.PublicationDelivered}}, 0,
			[]string{"retested on the moved default branch: promotion retest", "main commit: " + mainSHA, "result: passed"}},
		"retest running": {apicontract.PromotionWaitResult{Promotion: &model.Promotion{ID: "retest", Status: model.PromotionPending}, TimedOut: true}, 1,
			[]string{"The default branch moved; the promotion is being retested.", "promoting again is not needed", "oberth promote status retest"}},
	} {
		t.Run(name, func(t *testing.T) { checkFollowedRetest(t, tc.status, tc.code, tc.want) })
	}
}

// A followed retest that passed and is publishing at a timed-out wait is
// reported as publishing, not as being retested, and the command goes on.
func TestPromoteSaysAPassedRetestIsPublishing(t *testing.T) {
	mainSHA := strings.Repeat("a", 40)
	passed := &model.Promotion{ID: "retest", Status: model.PromotionPassed, ResultSHA: mainSHA}
	b := fake.Bindings()
	b.Promote.Handler = &fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult]{Value: apicontract.PromoteResult{ID: "promotion", TestedRunID: "tested-run"}}
	status := &fake.Handler[apicontract.PromoteStatusArgs, apicontract.PromotionWaitResult]{}
	status.Run = func(context.Context) (apicontract.PromotionWaitResult, error) {
		if _, _, calls := status.Observed(); calls == 1 {
			return apicontract.PromotionWaitResult{Promotion: passed, Publication: &model.Publication{ID: "publication:retest", Status: model.PublicationPending, Reason: "publication retry deferred"}, TimedOut: true}, nil
		}
		return apicontract.PromotionWaitResult{Promotion: passed, Publication: &model.Publication{ID: "publication:retest", Status: model.PublicationDelivered}}, nil
	}
	b.PromoteStatus.Handler = status
	b.Wait.Handler = &fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "ci-run", Status: stepmodel.RunPassed}}}
	app, out, diagnostic := fixture(t, b)
	app.git = func(context.Context, []string) (string, error) { return strings.Repeat("b", 40), nil }
	if code := app.run(context.Background(), []string{"promote", strings.Repeat("b", 40), "--repo", "project"}); code != 0 {
		t.Fatal("a publishing retest ended the command", code, diagnostic.String())
	}
	if !strings.Contains(out.String(), "promotion passed; publishing") || strings.Contains(out.String()+diagnostic.String(), "being retested") {
		t.Fatal("a passed retest was reported as being retested", out.String(), diagnostic.String())
	}
}

func checkFollowedRetest(t *testing.T, result apicontract.PromotionWaitResult, wantCode int, wants []string) {
	t.Helper()
	b := fake.Bindings()
	b.Promote.Handler = &fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult]{Value: apicontract.PromoteResult{ID: "promotion", TestedRunID: "tested-run"}}
	status := &fake.Handler[apicontract.PromoteStatusArgs, apicontract.PromotionWaitResult]{Value: result}
	b.PromoteStatus.Handler = status
	b.Wait.Handler = &fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "ci-run", Status: stepmodel.RunPassed}}}
	app, out, diagnostic := fixture(t, b)
	app.git = func(context.Context, []string) (string, error) { return strings.Repeat("b", 40), nil }
	code := app.run(context.Background(), []string{"promote", strings.Repeat("b", 40), "--repo", "project"})
	text := out.String() + diagnostic.String()
	if _, _, calls := status.Observed(); code != wantCode || calls != 1 {
		t.Fatal("wrong outcome", code, calls, text)
	}
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatal("missing", want, text)
		}
	}
	if strings.Contains(text, "promote again") {
		t.Fatal("asked to promote again", text)
	}
}

// A promotion whose retest could not start is reported as failed with its
// reason and cause, and the person is told to promote again.
func TestPromoteReportsARetestThatDidNotStart(t *testing.T) {
	b := fake.Bindings()
	b.Promote.Handler = &fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult]{Value: apicontract.PromoteResult{ID: "promotion", TestedRunID: "tested-run"}}
	conflict := "The source conflicts with the current default branch. Resolve the conflicts on a branch, pass CI and promote its exact commit."
	b.PromoteStatus.Handler = &fake.Handler[apicontract.PromoteStatusArgs, apicontract.PromotionWaitResult]{Value: apicontract.PromotionWaitResult{Promotion: &model.Promotion{ID: "promotion", Status: model.PromotionFailed, Reason: "default branch moved; retest not started", Detail: conflict}}}
	app, out, diagnostic := fixture(t, b)
	app.git = func(context.Context, []string) (string, error) { return strings.Repeat("b", 40), nil }
	if code := app.run(context.Background(), []string{"promote", strings.Repeat("b", 40), "--repo", "project"}); code != 1 {
		t.Fatal("a promotion that did not land succeeded", code, out.String())
	}
	text := strings.Join(strings.Fields(diagnostic.String()), " ")
	for _, want := range []string{"Promotion failed.", "promote again", "default branch moved; retest not started", "The source conflicts with the current default branch."} {
		if !strings.Contains(text, want) {
			t.Fatal("missing", want, diagnostic.String())
		}
	}
	if strings.Contains(text, "being retested") {
		t.Fatal("a retest that does not exist was reported", diagnostic.String())
	}
}

func TestInitAndValidateInRealCheckout(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.directory = t.TempDir()
	if _, err := runGit(context.Background(), app.directory, "init"); err != nil {
		t.Fatal(err)
	}
	if code := app.run(context.Background(), []string{"init"}); code != 0 {
		t.Fatalf("init %d: %s", code, diagnostic.String())
	}
	out.Reset()
	diagnostic.Reset()
	if code := app.run(context.Background(), []string{"validate"}); code != 0 {
		t.Fatalf("validate %d: %s", code, diagnostic.String())
	}
	if app.run(context.Background(), []string{"init"}) != 1 {
		t.Fatal("init overwrote the pipeline")
	}
	if err := os.WriteFile(app.directory+"/.oberth/sibling", []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if app.run(context.Background(), []string{"init", "--force"}) != 0 {
		t.Fatal("force init failed")
	}
	if data, err := os.ReadFile(app.directory + "/.oberth/sibling"); err != nil || string(data) != "keep" {
		t.Fatal("sibling changed")
	}
}

func TestFlowMessageCatalogue(t *testing.T) {
	for key, m := range flowMessages() {
		if publicerr.CheckMessage(m[0], 80) != nil || publicerr.CheckMessage(m[1], 120) != nil {
			t.Fatal("invalid flow message", key)
		}
	}
}

func TestReleaseTagsPushesAndWaits(t *testing.T) {
	b := fake.Bindings()
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Repositories: []apicontract.Repository{{Repo: "project", DefaultBranch: "main"}}}}
	b.Wait.Handler = &fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "release-run", Status: stepmodel.RunPassed}}}
	b.EventsWait.Handler = &fake.Handler[apicontract.EventsWaitArgs, apicontract.EventResult]{Value: apicontract.EventResult{EventPage: model.EventPage{Events: []model.AgentEvent{{ID: 42, Kind: string(model.EventReleasePublished), RunID: "release-run"}}}}}
	app, out, diagnostic := fixture(t, b)
	app.directory = t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.name", "Oberth"}, {"config", "user.email", "build@oberth.ci"}, {"commit", "--allow-empty", "-m", "first"}, {"config", "oberth.repository", "project"}} {
		if _, err := runGit(context.Background(), app.directory, args...); err != nil {
			t.Fatal(err)
		}
	}
	sha, err := runGit(context.Background(), app.directory, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	b.Wait.Handler = &fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "release-run", Status: stepmodel.RunPassed, CommitSHA: &sha}}}
	b.RunCheck.Handler = &fake.Handler[apicontract.RunCheckArgs, apicontract.RunCheckResult]{Value: apicontract.RunCheckResult{OK: true, ReleaseTag: "v0.1.0", CommitSHA: &sha}}
	// fixture captured the binding values; rebuild its transport for these results.
	fresh, _, _ := fixture(t, b)
	app.config.Transport = fresh.config.Transport
	if _, err = runGit(context.Background(), app.directory, "update-ref", "refs/remotes/oberth/main", sha); err != nil {
		t.Fatal(err)
	}
	var pushed string
	app.git = func(ctx context.Context, args []string) (string, error) {
		if args[0] == "fetch" {
			return "", nil
		}
		if args[0] == "push" {
			pushed = strings.Join(args, " ")
			return "", nil
		}
		return runGit(ctx, app.directory, args...)
	}
	if code := app.run(context.Background(), []string{"release", "v0.1.0"}); code != 0 {
		t.Fatalf("release %d: %s", code, diagnostic.String())
	}
	target, err := runGit(context.Background(), app.directory, "rev-parse", "refs/tags/v0.1.0^{commit}")
	if err != nil || target != sha || !strings.Contains(pushed, "refs/tags/v0.1.0:refs/tags/v0.1.0") || !strings.Contains(out.String(), "release.published: 42") {
		t.Fatal("release did not tag/push/wait correctly")
	}
}

func TestAuthoringRefusesSymlinksAndOversize(t *testing.T) {
	app, _, _ := fixture(t, fake.Bindings())
	app.directory = t.TempDir()
	if err := os.WriteFile(app.directory+"/real.yaml", []byte(starterWorkflow()), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.yaml", app.directory+"/linked.yaml"); err != nil {
		t.Fatal(err)
	}
	if app.run(context.Background(), []string{"validate", "linked.yaml"}) != 1 {
		t.Fatal("validate followed a link")
	}
	for _, size := range []int{1 << 20, 1<<20 + 1} {
		if err := os.WriteFile(app.directory+"/bounded", []byte(strings.Repeat("x", size)), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := readAuthored(app.directory, "bounded")
		if (err != nil) != (size > 1<<20) {
			t.Fatalf("bound %d: %v", size, err)
		}
	}
}

func TestValidateExplainsNativeRefusal(t *testing.T) {
	app, _, diagnostic := fixture(t, fake.Bindings())
	app.directory = t.TempDir()
	data := strings.Replace(starterWorkflow(), "entrypoint: build", "entrypoint: missing", 1)
	if err := os.WriteFile(app.directory+"/bad.yaml", []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	if code := app.run(context.Background(), []string{"validate", "bad.yaml"}); code != 1 || !strings.Contains(diagnostic.String(), "Workflow admission refused.") {
		t.Fatalf("native refusal hidden: %d %s", code, diagnostic.String())
	}
	diagnostic.Reset()
	if code := app.run(context.Background(), []string{"validate", "bad.yaml", "--detail"}); code != 1 || !strings.Contains(diagnostic.String(), "entrypoint") {
		t.Fatalf("native detail missing: %d %s", code, diagnostic.String())
	}
}

func TestLogDrainsBeforeTerminalSummary(t *testing.T) {
	b := fake.Bindings()
	logs := &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{}
	logs.Run = func(context.Context) (apicontract.LogResult, error) {
		inputs, _, _ := logs.Observed()
		request := inputs[len(inputs)-1]
		if request.ByteOffset == nil {
			return apicontract.LogResult{Content: "first\n", Meta: logcontract.Meta{HasMore: true, NextByteOffset: 6}}, nil
		}
		if *request.ByteOffset != 6 {
			t.Fatal("wrong byte cursor")
		}
		return apicontract.LogResult{Content: "second\n"}, nil
	}
	b.RunLogs.Handler = logs
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, stepmodel.RunSummary]{Value: stepmodel.RunSummary{ID: "run", Status: stepmodel.RunPassed, Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepPassed}}}}
	app, out, diagnostic := fixture(t, b)
	if code := app.run(context.Background(), []string{"log", "run"}); code != 0 || !strings.HasPrefix(out.String(), "== build ==\nfirst\nsecond\n") {
		t.Fatalf("log drain %d: %s %s", code, out.String(), diagnostic.String())
	}
}

func TestInviteDefaultsToPrivateCustody(t *testing.T) {
	b := fake.Bindings()
	b.UplinkInvite.Handler = &fake.Handler[apicontract.UplinkInviteArgs, model.Invitation]{Value: model.Invitation{Identity: "agent", ServerURL: "https://oberth.local"}}
	app, out, diagnostic := fixture(t, b)
	env, key := app.env, issuerKey(t, issuerSSH, issuerHostKeys)
	app.env = func(name string) string {
		if name == "OBERTH_CLIENT_KEY" {
			return key
		}
		return env(name)
	}
	if code := app.run(context.Background(), []string{"invite", "project"}); code != 0 {
		t.Fatalf("invite %d: %s", code, diagnostic.String())
	}
	path := strings.Split(out.String(), "\n")[1]
	data, err := client.ReadPrivate(path)
	if err != nil || !strings.Contains(string(data), `"secret"`) {
		t.Fatal("handoff not saved in private custody")
	}
	var invitation model.InvitationFile
	if err = json.Unmarshal(data, &invitation); err != nil || invitation.KnownHosts != issuerHostKeys {
		t.Fatal("handoff lost the issuer's host-key record", err)
	}
	if !strings.Contains(path, "/invitations/") || strings.Contains(out.String(), string(data)) {
		t.Fatal("handoff location or output is unsafe")
	}
}

func TestFailedReleaseStopsBeforePublicationWait(t *testing.T) {
	b := fake.Bindings()
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Repositories: []apicontract.Repository{{Repo: "project", DefaultBranch: "main"}}}}
	wait := &fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]{}
	wait.Run = func(context.Context) (apicontract.WaitResult, error) {
		inputs, _, _ := wait.Observed()
		state := stepmodel.RunPassed
		if inputs[len(inputs)-1].Ref != "" {
			state = stepmodel.RunFailed
		}
		return apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "failed-release", Status: state, CommitSHA: ptrReleaseSHA()}}, nil
	}
	b.Wait.Handler = wait
	b.RunCheck.Handler = &fake.Handler[apicontract.RunCheckArgs, apicontract.RunCheckResult]{Value: apicontract.RunCheckResult{OK: true, ReleaseTag: "v0.1.0", CommitSHA: ptrReleaseSHA()}}
	b.EventsWait.Handler = &fake.Handler[apicontract.EventsWaitArgs, apicontract.EventResult]{PrepareError: publicerr.New(publicerr.InvalidInput, "Unexpected publication wait.", "Stop before waiting for publication.", nil)}
	app, _, diagnostic := fixture(t, b)
	app.git = func(_ context.Context, args []string) (string, error) {
		if args[0] == "config" {
			return "project", nil
		}
		return strings.Repeat("a", 40), nil
	}
	if code := app.run(context.Background(), []string{"release", "v0.1.0"}); code != 1 || !strings.Contains(diagnostic.String(), "Release run or publication failed.") {
		t.Fatal("failed release kept waiting", code, diagnostic.String())
	}
	_, _, calls := b.EventsWait.Handler.(*fake.Handler[apicontract.EventsWaitArgs, apicontract.EventResult]).Observed()
	if calls != 0 {
		t.Fatal("waited for impossible publication")
	}
}

func ptrReleaseSHA() *string { value := strings.Repeat("a", 40); return &value }
