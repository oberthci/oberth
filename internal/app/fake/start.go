package fake

import (
	"context"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/service"
)

type StartResolver struct {
	Resolve func(context.Context, service.StartRequest) (app.Request, error)
}

func (f StartResolver) ResolveStart(ctx context.Context, q service.StartRequest) (app.Request, error) {
	if err := ctx.Err(); err != nil {
		return app.Request{}, err
	}
	q.Request = q.Request.Clone()
	r, err := f.Resolve(ctx, q)
	r.Actor = r.Actor.Clone()
	return r, err
}
