package argojob_test

import (
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"testing"
	"time"
)

type outageNative struct {
	*fake.Workflows
	now       *time.Time
	recovered time.Time
	reads     int
}

func (f *outageNative) Get(ctx context.Context, name string, o meta.GetOptions) (*wf.Workflow, error) {
	f.reads++
	if f.now.Before(f.recovered) {
		return nil, errors.New("API disconnected")
	}
	f.Object.Status.Phase = wf.WorkflowSucceeded
	return f.Workflows.Get(ctx, name, o)
}

type advancingBackoff struct {
	workqueue.TypedRateLimiter[string]
	now    *time.Time
	delays []time.Duration
}

func (l *advancingBackoff) When(key string) time.Duration {
	delay := l.TypedRateLimiter.When(key)
	l.delays = append(l.delays, delay)
	*l.now = l.now.Add(delay)
	return time.Nanosecond
}
func TestAPIOutageBeyondOneMinuteKeepsRunUntilActualResult(t *testing.T) {
	t.Parallel()
	c, f, _ := observationFixture(t)
	now := time.Unix(100, 0)
	native := &outageNative{Workflows: f, now: &now, recovered: now.Add(time.Minute)}
	c.Workflows = native
	limiter := &advancingBackoff{TypedRateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Second, 8*time.Second), now: &now}
	c.RateLimiter = limiter
	c.Now = func() time.Time { return now }
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	reports := 0
	c.Reporter = &reportSpy{FinishedFunc: func(_ context.Context, _ string, o argojob.Observation) error {
		reports++
		if now.Before(native.recovered) || o.Status != stepmodel.RunPassed {
			t.Error("invented failure during outage", now, o)
		}
		cancel()
		return nil
	}}
	_ = argojob.New(c).Run(ctx)
	if reports != 1 || native.reads < 10 || now.Before(native.recovered) {
		t.Fatal(reports, native.reads, now)
	}
	for i, d := range limiter.delays {
		want := min(time.Second<<min(i, 3), 8*time.Second)
		if d != want {
			t.Fatal("backoff", limiter.delays)
		}
	}
}
func TestFindOutageIsUnavailableAndForeignWorkflowCarriesRefusal(t *testing.T) {
	t.Parallel()
	p, _, f := fixture(t)
	s := memoryAdmission(t, p)
	f.GetError = errors.New("API down")
	r, err := s.Run(t.Context(), p.View().RunID)
	compositionOK(t, err)
	_, found, err := argojob.New(argojob.Config{Runs: s, Workflows: f}).Find(t.Context(), r)
	if found || err == nil {
		t.Fatal("outage became absence", found, err)
	}
	f.GetError = nil
	_, err = argojob.New(argojob.Config{Runs: bindingRefusal{s}, Workflows: f}).Create(t.Context(), p)
	if err == nil {
		t.Fatal("fixture unexpectedly bound")
	}
	for _, label := range []string{nativebinding.EngineLabel, nativebinding.RunLabel} {
		old := f.Object.Labels[label]
		f.Object.Labels[label] = "foreign"
		_, found, err = argojob.New(argojob.Config{Runs: s, Workflows: f}).Find(t.Context(), r)
		var foreign *nativebinding.Refusal
		if found || !errors.As(err, &foreign) {
			t.Fatal("foreign object became retryable", err)
		}
		f.Object.Labels[label] = old
	}
}
