//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

// A server that now refuses a failed run's workflow refuses its rerun with the
// findings a push of that commit prints, on the command line and as a tool.
func TestComposedRefusedRerunListsThePushFindings(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	if pushed, err := f.pushResult(t, "refs/heads/later-refused", "-o", "oberth.run=1"); err != nil {
		t.Fatal("push refused", pushed, err)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	f.deliverSource(t, run)
	f.endNative(t, run, 1)
	f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunFailed })
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("first server did not stop")
	}
	raw, _ := json.Marshal(serverRuntime{Routes: workflowpolicy.ReleaseRoutes{gittest.Repository: {{Pattern: "v*", File: ".oberth/release.yaml"}}}, GitBinary: f.git.Binary, SourceTimeoutSeconds: 15, AdmissionTimeoutSeconds: 10, SourceClientBurst: 30, DefaultBranch: "refs/heads/main", ImagePrefixes: []string{"registry.example/"}})
	f.config.Runtime = raw
	f.restartChannels(t)
	_, restarted := f.run(t)
	f.checkStarting(t, restarted)
	f.waitStarted(t, restarted)
	f.bindClient(t)
	pushed, err := f.pushResult(t, "refs/heads/later-refused-again", "-o", "oberth.run=1")
	if err == nil || !strings.Contains(pushed, "\n  fix: ") {
		t.Fatal("stricter server admitted the commit", pushed)
	}
	findings := pushFindings(t, pushed, "refs/heads/later-refused-again", "refs/heads/later-refused")
	app, out, diagnostic := f.revokeApplication(t)
	if code := app.run(t.Context(), []string{"run", "rerun", run.ID}); code != 1 || out.Len() != 0 {
		t.Fatal("refused rerun did not fail", code, out.String(), diagnostic.String())
	}
	refusal, listed, _ := strings.Cut(diagnostic.String(), "Fix every reported finding, then push again.\n")
	if refusal != "pipeline admission failed\n" || strings.Join(strings.Fields(listed), " ") != strings.Join(strings.Fields(findings), " ") {
		t.Fatalf("rerun refusal does not list the push findings\nrerun: %s\npush: %s", diagnostic.String(), findings)
	}
	f.checkRerunToolFindings(t, run.ID, findings)
}

// pushFindings returns the finding lines of one refused ref, named as the rerun names it.
func pushFindings(t *testing.T, pushed, pushedRef, rerunRef string) string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(pushed, "\n") {
		line = strings.TrimPrefix(line, "remote: ")
		if strings.HasPrefix(line, pushedRef+" ") || strings.HasPrefix(line, "  fix: ") {
			lines = append(lines, strings.Replace(line, pushedRef, rerunRef, 1))
		}
	}
	if len(lines) < 2 {
		t.Fatal("push printed no findings", pushed)
	}
	return strings.Join(lines, "\n") + "\n"
}

func (f *composedFixture) checkRerunToolFindings(t *testing.T, id, findings string) {
	t.Helper()
	definition := apicontract.Definitions(apicontract.Bindings{})
	for _, d := range definition {
		if d.Name != "run_rerun" {
			continue
		}
		data, refused, err := f.administratorClient(t).Call(t.Context(), d, json.RawMessage(`{"id":"`+id+`"}`))
		var reply struct{ Reason, Remedy string }
		var wire struct{ Context []string }
		if err != nil || !refused || json.Unmarshal(data, &reply) != nil || json.Unmarshal(data, &wire) != nil {
			t.Fatal("tool rerun was not refused", string(data), refused, err)
		}
		if reply.Reason != "pipeline admission failed" || len(wire.Context) != 1 || wire.Context[0]+"\n" != findings {
			t.Fatalf("tool refusal does not list the push findings\ntool: %s\npush: %s", data, findings)
		}
		return
	}
	t.Fatal("run_rerun is not declared")
}
