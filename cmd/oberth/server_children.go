package main

import (
	"context"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
)

// The native executor registers before watches start; the syncer is assembled
// afterward and is available before any child notice can be consumed.
type scheduleChildren struct{ c *composition }

func (s scheduleChildren) ChildBinding(ctx context.Context, w *wf.Workflow) (nativebinding.ChildBinding, bool, bool, error) {
	return s.c.schedules.ChildBinding(ctx, w)
}
func (s scheduleChildren) VerifyChild(w *wf.Workflow, proof nativebinding.ChildBinding) error {
	return s.c.schedules.VerifyChild(w, proof)
}

// Initial child notices must see the synchronized cache before recovery drains them.
func nativeAfterWatch(ready, run func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := ready(ctx); err != nil {
			return err
		}
		return run(ctx)
	}
}
