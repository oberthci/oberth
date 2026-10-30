package app

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type rcaEvidenceController struct {
	configuredController
	completion argojob.Completion
	failure    error
}

func (c *rcaEvidenceController) Wait(context.Context, string, string, io.Writer) (argojob.Completion, error) {
	return c.completion, c.failure
}
func TestRCARevisionPartialWorkflowEvidenceDoesNotCollectLiveArtifacts(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "terminal"}[terminal], func(t *testing.T) {
			now := time.Now()
			failure := errors.New("identity delivery interrupted")
			controller := &rcaEvidenceController{failure: failure, completion: argojob.Completion{Name: "workflow", Terminal: terminal, ReplayAttempts: []argojob.StepResult{{Burn: "ci", Step: "earlier", Status: runprogress.StepPassed, StartedAt: &now}}}}
			collector, artifacts := &fakeCollector{body: []byte("archive")}, &fakeArtifactStore{}
			jobs := &ArgoJobs{controller: controller, intents: map[string]argoIntent{"workflow": {runID: "run", trigger: periapsis.TriggerCI, created: true}}, collector: collector, artifacts: artifacts, artifactLimit: 1 << 20}
			result, err := jobs.Wait(t.Context(), "workflow", io.Discard)
			if !errors.Is(err, failure) || !result.ObservedStepStart {
				t.Fatal("partial evidence erased", result, err)
			}
			if (collector.calls > 0) != terminal {
				t.Fatal("live artifact collection or skipped terminal archive", terminal, collector.calls)
			}
		})
	}
}
func TestRCARevisionWriterWindowDoesNotAcquireSecondReadinessGrace(t *testing.T) {
	health := Health{Store: fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}}, Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{}}
	health.refreshReadiness(t.Context())
	health.WriterReady = func() error { return writerBusyReadinessError(5) }
	health.refreshReadiness(t.Context())
	if err := health.Ready(t.Context()); err == nil || !errors.Is(err, errReadinessWriter) {
		t.Fatal("writer window received another green grace", err)
	}
	if health.Readiness.retainedBusy != 0 {
		t.Fatal("writer error counted as retained reader busy")
	}
}
