package service

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
)

// An upstream whose default branch Oberth cannot determine is a forge
// configuration problem: the reason must not call it infrastructure, the
// hint must not advise an unchanged retry, and the scheduler must not
// auto-retry it.
func TestUpstreamDefaultBranchFailureIsConfigurationNotInfrastructure(t *testing.T) {
	t.Parallel()
	message := "prepare repository: " + gitcache.ErrUpstreamDefaultBranch.Error() + ": the upstream HEAD resolves to a commit"
	run := model.Run{ID: "run", Status: model.RunFailed, Phase: "infrastructure", Error: message, RefKind: model.RefBranch, Trigger: "push"}
	if reason := failedRunDisplayReason(run); strings.HasPrefix(reason, "infrastructure") || !strings.HasPrefix(reason, "upstream configuration error: ") {
		t.Fatalf("reason = %q", reason)
	}
	hint := runRetryHint(run, nil)
	if hint == nil || hint.Class != model.RetryCode || hint.Cause != "upstream_configuration" {
		t.Fatalf("hint = %+v", hint)
	}
	// The scheduler retries an otherwise identical transport failure within budget,
	// and never the configuration failure.
	logs, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run.ID = strings.Repeat("a", 32)
	if err := logs.InitializeStepProgress(run.ID); err != nil {
		t.Fatal(err)
	}
	scheduler := &Scheduler{logs: logs}
	result := JobResult{Status: model.RunFailed, Phase: "infrastructure", Error: "prepare repository: connect to upstream: connection refused"}
	if scheduler.infrastructureAutoRetry(run, result) == 0 {
		t.Fatal("a transport failure outside any step is not auto-retried; the fixture proves nothing")
	}
	result.Error = message
	if scheduler.infrastructureAutoRetry(run, result) != 0 {
		t.Fatal("an upstream configuration failure was auto-retried")
	}
}
