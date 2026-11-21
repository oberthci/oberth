package apicontract

import (
	"context"
	"github.com/oberthci/oberth/internal/actor"
)

// Caller binds a call to live credentials. CredentialID never appears on the wire.
type Caller struct {
	Actor        actor.Actor
	CredentialID string
	AuditTarget  string
}

// Call observes or executes an already authorized typed operation once. Durable
// actions own their lifetime; canceling observation never cancels accepted work.
type Call[O any] func(context.Context) (O, error)

// Handler is implemented by the service for each concrete input/result pair.
// Prepare checks live authority and resolves selectors in scope before streaming;
// audit intent/outcome and the mutation gate remain with the service.
// Results are authorized, redacted public values; native detail stays separate.
// Prepare and Call stop promptly on cancellation and join owned work before
// returning. Required result arrays are explicit empty slices, never nil.
// Accepted durable actions belong to C2 beyond this observation context.
type Handler[I, O any] interface {
	Prepare(context.Context, Caller, I) (Call[O], error)
}

// HandlerFunc binds a named service method without an untyped dispatcher.
type HandlerFunc[I, O any] func(context.Context, Caller, I) (Call[O], error)

func (f HandlerFunc[I, O]) Prepare(ctx context.Context, c Caller, input I) (Call[O], error) {
	return f(ctx, c, input)
}
