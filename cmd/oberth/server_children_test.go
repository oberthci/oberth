package main

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestNativeObserverWaitsForWatchReadiness(t *testing.T) {
	for _, waitErr := range []error{nil, context.Canceled} {
		t.Run(map[bool]string{true: "ready", false: "canceled"}[waitErr == nil], func(t *testing.T) {
			var calls []string
			stopped := errors.New("native observer stopped")
			ready := func(context.Context) error { calls = append(calls, "watch"); return waitErr }
			run := func(context.Context) error { calls = append(calls, "native"); return stopped }
			err := nativeAfterWatch(ready, run)(t.Context())
			if waitErr != nil {
				if !errors.Is(err, waitErr) || !slices.Equal(calls, []string{"watch"}) {
					t.Fatal("native started before its watch", calls, err)
				}
			} else if !errors.Is(err, stopped) || !slices.Equal(calls, []string{"watch", "native"}) {
				t.Fatal("native lifetime was lost", calls, err)
			}
		})
	}
}

func TestAssemblyGatesNativeObserverOnWatchReadiness(t *testing.T) {
	f := fixtureServer(t)
	c := testComposition(t)
	c.config = f.config
	life := newLifecycle(t.Context(), nil)
	defer func() {
		if err := life.stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	builders := f.deps.constructors
	build := builders.NativeNew
	called := false
	builders.NativeNew = func(ctx context.Context, c *composition) (Part, error) {
		part, err := build(ctx, c)
		part.Run = func(context.Context) error { called = true; return errors.New("native ran") }
		return part, err
	}
	if err := assemble(t.Context(), c, life, builders); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	found := false
	for _, part := range life.parts {
		if part.Name != "native" {
			continue
		}
		found = true
		if err := part.Run(ctx); !errors.Is(err, context.Canceled) || called {
			t.Fatal("assembled native observer bypassed watch readiness", called, err)
		}
	}
	if !found {
		t.Fatal("assembled native part missing")
	}
}
