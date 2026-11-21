package fake

import (
	"context"
	"crypto/x509"
	"github.com/oberthci/oberth/internal/apicontract"
	"sync"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/publicerr"
)

type Authenticator struct {
	mu     sync.Mutex
	caller apicontract.Caller
	err    error
}

func NewAuth(c apicontract.Caller) *Authenticator { return &Authenticator{caller: c} }
func (a *Authenticator) Set(c apicontract.Caller, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.caller = c
	a.err = err
}
func (a *Authenticator) result(ctx context.Context) (apicontract.Caller, error) {
	if err := ctx.Err(); err != nil {
		return apicontract.Caller{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.caller
	c.Actor = c.Actor.Clone()
	return c, a.err
}
func (a *Authenticator) Bearer(ctx context.Context, token string) (apicontract.Caller, error) {
	if token != "valid" {
		return apicontract.Caller{}, publicerr.New(publicerr.Forbidden, "Invalid credentials.", "Use a current credential.", nil)
	}
	return a.result(ctx)
}
func (a *Authenticator) Certificate(ctx context.Context, _ *x509.Certificate) (apicontract.Caller, error) {
	return a.result(ctx)
}

type Diagnostics struct {
	mu    sync.Mutex
	items []api.Diagnostic
}

func (d *Diagnostics) Record(v api.Diagnostic) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.items = append(d.items, v)
}
func (d *Diagnostics) Items() []api.Diagnostic {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]api.Diagnostic(nil), d.items...)
}
