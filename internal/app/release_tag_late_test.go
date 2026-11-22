package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitport"
)

// gateDeadline lets a test end the gate's own deadline at a chosen moment.
func gateDeadline(f *fixture) *context.CancelFunc {
	var expire context.CancelFunc
	f.d.Deadline = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithTimeout(parent, d)
		expire = cancel
		return ctx, cancel
	}
	return &expire
}

func TestGatePassesOnceTheTagIsDeliveredWhateverHappensNext(t *testing.T) {
	t.Parallel()
	for name, late := range map[string]func(*app.Submitter, *context.CancelFunc){
		"deadline passes": func(_ *app.Submitter, expire *context.CancelFunc) { (*expire)() },
		"server closes":   func(s *app.Submitter, _ *context.CancelFunc) { _ = s.Close(context.Background()) },
	} {
		t.Run(name, func(t *testing.T) {
			f, r := releaseFixture(t)
			expire := gateDeadline(f)
			s := f.new(t)
			forge := &tagForge{f: f, receive: r.ID, after: func() { late(s, expire) }}
			r.Forge = forge
			if _, err := s.Gate(context.Background(), r); err != nil || len(forge.delivered) != 1 {
				t.Fatal("a delivered release tag was refused, spending its version with no run", err)
			}
		})
	}
}

func TestGateRefusesBeforeDeliveryWhenItCannotPass(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		before func(*app.Submitter, *context.CancelFunc)
		want   string
	}{
		"deadline passed": {func(_ *app.Submitter, expire *context.CancelFunc) { (*expire)() }, "admission timed out"},
		"server closing":  {func(s *app.Submitter, _ *context.CancelFunc) { _ = s.Close(context.Background()) }, "server is shutting down"},
	} {
		t.Run(name, func(t *testing.T) {
			f, r := releaseFixture(t)
			expire := gateDeadline(f)
			s := f.new(t)
			forge := &tagForge{f: f, receive: r.ID}
			r.Forge = forge
			read := f.source.Read
			f.source.Read = func(ctx context.Context, rev gitport.Revision, path string) (gitport.File, error) {
				tc.before(s, expire)
				return read(ctx, rev, path)
			}
			r.Source = f.source
			_, err := s.Gate(context.Background(), r)
			if err == nil || len(forge.delivered) != 0 {
				t.Fatal("a gate that cannot pass delivered the tag", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatal(err)
			}
		})
	}
}
