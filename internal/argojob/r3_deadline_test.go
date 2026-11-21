package argojob

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/model"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"

	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type deadlineNative struct {
	*reviewR2Native
	healthy nativebinding.RunBinding
}

func (n *deadlineNative) Get(ctx context.Context, name string, opts meta.GetOptions) (*wf.Workflow, error) {
	w, err := n.reviewR2Native.Get(ctx, name, opts)
	if name == n.healthy.Workflow.Name {
		w.Name, w.UID = name, "healthy-uid"
		stampSubmission(w, admissioncontract.PreparedView{RunID: n.healthy.RunID, Ref: n.healthy.Ref, CommitSHA: n.healthy.Revision.Commit, SpecDigest: n.healthy.SpecSHA256, Trigger: "branch"}, n.healthy.RepositoryID)
		for id, node := range w.Status.Nodes {
			node.Name = name + ".build"
			w.Status.Nodes[id] = node
		}
	}
	return w, err
}

type deadlineReporter struct {
	bad              string
	cancel           context.CancelFunc
	blocked, healthy bool
	err              error
}

func (r *deadlineReporter) Finished(ctx context.Context, id string, _ Observation) error {
	if id == r.bad {
		r.blocked = true
		<-ctx.Done()
		r.err = ctx.Err()
		return r.err
	}
	if id != "healthy" {
		return nil
	}
	r.healthy = true
	r.cancel()
	return nil
}
func (*deadlineReporter) Held(context.Context, model.HeldNotice) error { return nil }

type deadlineProgress struct {
	*reviewR2Progress
	bad     string
	err     error
	blocked bool
}

func (p *deadlineProgress) SaveSteps(ctx context.Context, id string, _ []stepmodel.RunStep) error {
	if id == p.bad {
		p.blocked = true
		<-ctx.Done()
		p.err = ctx.Err()
		return p.err
	}
	return nil
}
func TestObservationDeadlineLetsAnotherRunProceed(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"Finished", "SaveSteps"} {
		t.Run(operation, func(t *testing.T) {
			e, tr, native, _ := reviewR2Executor(t, &reviewR2Reporter{})
			e.config.ObservationTimeout = 20 * time.Millisecond
			b := tr.binding
			b.RunID = "healthy"
			b.Workflow.Name = "healthy"
			b.Workflow.UID = "healthy-uid"
			e.config.Workflows = &deadlineNative{native, b}
			e.track(b, tr.plan, trackingSeed{})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			reporter := &deadlineReporter{cancel: cancel}
			progress := &deadlineProgress{reviewR2Progress: &reviewR2Progress{}}
			if operation == "Finished" {
				reporter.bad = tr.binding.RunID
			} else {
				progress.bad = tr.binding.RunID
			}
			e.config.Reporter, e.config.Progress = reporter, progress
			_ = e.Run(ctx)
			err, blocked := reporter.err, reporter.blocked
			if operation == "SaveSteps" {
				err, blocked = progress.err, progress.blocked
			}
			if !blocked || !reporter.healthy || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("worker blocked another run", blocked, reporter.healthy, err)
			}
		})
	}
}

type boundedCapture struct {
	err error
}

func (s *boundedCapture) Reserve(ctx context.Context, _ string, _ runlog.ContainerLog) (*runlog.Reservation, error) {
	<-ctx.Done()
	s.err = ctx.Err()
	return nil, s.err
}
func TestLateCaptureUsesOnlyRemainingEvidenceWindow(t *testing.T) {
	t.Parallel()
	now := time.Unix(100, 0)
	store := &boundedCapture{}
	e := New(Config{Logs: store, Now: func() time.Time { return now }})
	defer e.queue.ShutDown()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	e.captureBatch(ctx, captureJob{until: now.Add(20 * time.Millisecond), entries: []runlog.ContainerLog{{PodUID: "uid", Container: "main"}}})
	if !errors.Is(store.err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatal("capture outlived evidence window", ctx.Err(), store.err)
	}
}

func TestAcknowledgedObservationUsesOnlyRemainingWindow(t *testing.T) {
	t.Parallel()
	e, tr, _, now := reviewR2Executor(t, &reviewR2Reporter{})
	tr.completed = true
	tr.evidenceUntil = now.Add(20 * time.Millisecond)
	progress := &deadlineProgress{reviewR2Progress: &reviewR2Progress{}, bad: tr.binding.RunID}
	e.config.Progress = progress
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	e.process(ctx, tr.binding.Workflow.Name)
	if !errors.Is(progress.err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatal("observation exceeded evidence deadline", ctx.Err(), progress.err)
	}
}
