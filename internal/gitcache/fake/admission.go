package fake

import (
	"context"
	"github.com/oberthci/oberth/internal/gitcache"
)

// Admission models a cooperative producer waiting for its preparation context.
type Admission struct{ Entered chan<- struct{} }

func (a Admission) Check(ctx context.Context, _ gitcache.ReceiveEvent, _ *gitcache.ReceiveSource) error {
	close(a.Entered)
	<-ctx.Done()
	return context.Cause(ctx)
}
