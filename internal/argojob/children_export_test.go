package argojob

import (
	"context"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
)

func (e *Executor) RecordChildForTest(ctx context.Context, w *wf.Workflow) error {
	return e.recordChild(ctx, w)
}
func (e *Executor) ProcessForTest(ctx context.Context, name string) { e.process(ctx, name) }

func (e *Executor) ChildRetriesForTest(name string) int { return e.queue.NumRequeues(name) }
func (e *Executor) ChildNoticesForTest() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.childNotices)
}
func (e *Executor) AcceptChildForTest(ctx context.Context, p *admissioncontract.PreparedAdmission, proof nativebinding.ChildBinding) error {
	return e.acceptChild(ctx, p, proof)
}
func CheckChildMessagesForTest() error {
	for _, s := range []string{childNotRecorded, childAdmissionInvalid, childAdmissionPending, childObserved} {
		if err := publicerr.CheckMessage(s, 80); err != nil {
			return err
		}
	}
	return publicerr.CheckMessage(childRemedy, 120)
}

func (e *Executor) RecoverChildrenForTest(ctx context.Context) error { return e.recover(ctx) }
