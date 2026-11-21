package contracttest

import (
	"context"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/service"
)

type DeciderFixture struct {
	Port     app.Service
	Update   service.RefUpdate
	Decision service.Decision
}

func Decider(t *testing.T, setup func(*testing.T) DeciderFixture) {
	t.Helper()
	t.Run("decider", func(t *testing.T) {
		f := setup(t)
		got, err := f.Port.Decide(context.Background(), f.Update)
		if err != nil || !reflect.DeepEqual(got, f.Decision) {
			t.Fatal(got, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := f.Port.Decide(ctx, f.Update); err == nil {
			t.Fatal("cancelled decision succeeded")
		}
	})
}
