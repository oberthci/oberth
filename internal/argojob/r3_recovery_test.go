package argojob_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/stateport"
	"k8s.io/client-go/util/workqueue"
)

type recoveringCatalog struct {
	recoveredOwner
	calls int
}

func (r *recoveringCatalog) Runs(ctx context.Context, q stateport.RunFilter) ([]stateport.Run, error) {
	r.calls++
	if r.calls <= 8 {
		return nil, errors.New("catalog unavailable")
	}
	return r.recoveredOwner.Runs(ctx, q)
}
func TestRecoveryListingRefusalRetriesWithoutExiting(t *testing.T) {
	t.Parallel()
	c, f, _ := observationFixture(t)
	owner := &recoveringCatalog{recoveredOwner: c.Runs}
	c.Runs = owner
	f.Object.Status.Phase = wf.WorkflowSucceeded
	c.Cache = &fake.Cache{WorkflowObject: f.Object}
	c.RateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Nanosecond, time.Nanosecond)
	var log bytes.Buffer
	c.Logger = slog.New(slog.NewJSONHandler(&log, nil))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	reported := 0
	c.Reporter = &reportSpy{FinishedFunc: func(context.Context, string, argojob.Observation) error { reported++; cancel(); return nil }}
	_ = argojob.New(c).Run(ctx)
	if owner.calls < 9 || reported != 1 || !strings.Contains(log.String(), "catalog unavailable") {
		t.Fatal("catalog error ended recovery", owner.calls, reported, log.String())
	}
}
