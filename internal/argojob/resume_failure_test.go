package argojob_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestResumeStartFailureSettlesAcceptedRun(t *testing.T) {
	t.Parallel()
	s, in, native, pods := resumeRecoveryFixture(t)
	cause := errors.New("native read unavailable")
	native.GetError = cause
	config := resumeRecoveryConfig(s, native, pods)
	config.Reporter = &reportSpy{FinishedFunc: func(ctx context.Context, id string, o argojob.Observation) error {
		return saveResumeFinish(ctx, s, id, o)
	}}
	if err := argojob.New(config).ApplyResume(t.Context(), in.RunID); !errors.Is(err, cause) {
		t.Fatal("native failure lost", err)
	}
	r, err := s.Run(t.Context(), in.RunID)
	compositionOK(t, err)
	if r.Status != stepmodel.RunFailed || r.DisplayReason != "resume could not start" || r.Detail != cause.Error() {
		t.Fatal("accepted resume did not settle", r.Status, r.DisplayReason, r.Detail)
	}
	if native.Patches != 0 || pods.Deletes != 0 {
		t.Fatal("failed native read changed the Workflow or Pods")
	}
}
