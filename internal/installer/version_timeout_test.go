package installer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/installer"
)

func TestVersionMismatchSurvivesEveryWaitBoundary(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"pause", "probe", "next iteration"} {
		t.Run(boundary, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { versionExpiryCase(t, boundary) })
		})
	}
}

func versionExpiryCase(t *testing.T, boundary string) {
	t.Helper()
	f := setup(t)
	p := prepare(t, f)
	f.engine.Timeout = time.Minute
	waiting, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	f.engine.CheckEndpoint = func(_ context.Context, profile installer.Profile) error {
		if boundary == "probe" {
			<-waiting.Done()
		}
		return installer.NewProblem("ready-version", nil, "Server version: v0.0.9\nReviewed release: "+profile.ServerVersion)
	}
	if boundary == "next iteration" {
		f.engine.Pause = func(ctx context.Context) error { <-ctx.Done(); return nil }
	}
	_, err := f.engine.Apply(waiting, p, p.Digest(), nil)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "version-timeout" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("version mismatch became a generic deadline", err)
	}
	if refusal.Reason != "Server still serves another version." || len([]rune(refusal.Reason)) > 40 || len([]rune(refusal.Remedy)) > 120 {
		t.Fatal("version refusal is not short and complete", refusal)
	}
	for _, fact := range []string{"Server version: v0.0.9", "Reviewed release: unset", "oberth system status"} {
		if !strings.Contains(err.Error(), fact) {
			t.Fatal("version refusal lost its facts or remedy", fact, err)
		}
	}
}

func TestLatestReplyDecidesReadinessDeadlineReason(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"connect", "starting", ""} {
		t.Run(code, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := setup(t)
				p := prepare(t, f)
				waiting, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				checks := 0
				f.engine.CheckEndpoint = func(context.Context, installer.Profile) error {
					checks++
					if checks == 1 {
						return installer.NewProblem("ready-version", nil, "Server version: v0.0.9")
					}
					<-waiting.Done()
					if code == "" {
						return nil
					}
					return installer.NewProblem(code, waiting.Err(), "")
				}
				f.engine.Pause = func(context.Context) error { return nil }
				_, err := f.engine.Apply(waiting, p, p.Digest(), nil)
				var refusal *installer.Problem
				if !errors.As(err, &refusal) || refusal.Code != "timeout" || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("earlier version mismatch hid the latest reply", err)
				}
			})
		})
	}
}
