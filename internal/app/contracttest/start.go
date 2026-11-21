package contracttest

import (
	"context"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/service"
)

type StartResolverFixture struct {
	Port     app.StartResolver
	Request  service.StartRequest
	Expected app.Request
}

func StartResolver(t *testing.T, setup func(*testing.T) StartResolverFixture) {
	t.Helper()
	t.Run("start-resolver", func(t *testing.T) {
		f := setup(t)
		got, err := f.Port.ResolveStart(t.Context(), f.Request)
		if err != nil || !reflect.DeepEqual(got, f.Expected) {
			t.Fatal("resolver changed exact request facts", got, err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := f.Port.ResolveStart(ctx, f.Request); err == nil {
			t.Fatal("resolver ignored cancellation")
		}
	})
}
