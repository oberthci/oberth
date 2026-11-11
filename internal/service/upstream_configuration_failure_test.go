package service

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
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
	// infrastructureAutoRetry refuses exactly what this guard matches.
	if !upstreamConfigurationFailure(message) || upstreamConfigurationFailure("connect to upstream: connection refused") {
		t.Fatal("the auto-retry guard does not separate configuration from transport failures")
	}
}
