package installer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
)

type idleAdminFunc func(context.Context, string, any, any) error

func (f idleAdminFunc) Admin(ctx context.Context, action string, args, into any) error {
	return f(ctx, action, args, into)
}
func (f idleAdminFunc) DownloadAdminBackup(context.Context, int64, io.Writer) error {
	return errors.New("unexpected download")
}

func TestAdminIdleObservation(t *testing.T) {
	t.Run("BusyThenIdleAndCriticalSelection", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := 0
			var out bytes.Buffer
			deps := Deps{Output: &out, PollInterval: time.Second, Admin: idleAdminFunc(func(ctx context.Context, action string, args, into any) error {
				calls++
				if action != "idle" || args.(map[string]string)["wait_for"] != "critical" {
					t.Fatal(action, args)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("missing deadline")
				}
				busy := calls == 1
				queue := model.IdleQueueStatus{Version: 1}
				if busy {
					queue.Running = 1
					queue.Tag = 1
				}
				return fixtureDecode(map[string]any{"busy": busy, "queue": queue}, into)
			})}
			if err := observeAdminIdle(t.Context(), "critical", time.Minute, deps); err != nil || calls != 2 || !strings.Contains(out.String(), "critical=1") {
				t.Fatal(err, calls, out.String())
			}
		})
	})
	t.Run("TransientThenRecovery", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := 0
			deps := Deps{Output: io.Discard, PollInterval: time.Second, Admin: idleAdminFunc(func(_ context.Context, _ string, _ any, into any) error {
				calls++
				if calls == 1 {
					return &client.AdminError{StatusCode: 503, Cause: errors.New("temporarily unavailable")}
				}
				return fixtureDecode(map[string]any{"busy": false, "queue": model.IdleQueueStatus{Version: 1}}, into)
			})}
			if err := observeAdminIdle(t.Context(), "all", 0, deps); err != nil || calls != 2 {
				t.Fatal(err, calls)
			}
		})
	})
	t.Run("AuthorizationStopsImmediately", func(t *testing.T) {
		calls := 0
		deps := Deps{Output: io.Discard, Admin: idleAdminFunc(func(context.Context, string, any, any) error {
			calls++
			return &client.AdminError{StatusCode: http.StatusForbidden, Cause: errors.New("forbidden")}
		})}
		if err := observeAdminIdle(t.Context(), "all", 0, deps); err == nil || calls != 1 {
			t.Fatal(err, calls)
		}
	})
	t.Run("OwnDeadlineAndCancellation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			deps := Deps{Output: io.Discard, Admin: idleAdminFunc(func(ctx context.Context, _ string, _ any, _ any) error { <-ctx.Done(); return ctx.Err() })}
			err := observeAdminIdle(t.Context(), "all", 3*time.Minute, deps)
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 3*time.Minute {
				t.Fatal(err, time.Since(start))
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			err = observeAdminIdle(ctx, "all", 0, deps)
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	})
	t.Run("InvalidObservationRefused", func(t *testing.T) {
		deps := Deps{Admin: idleAdminFunc(func(_ context.Context, _ string, _ any, into any) error {
			return fixtureDecode(map[string]any{"busy": false, "queue": model.IdleQueueStatus{}}, into)
		})}
		if err := observeAdminIdle(t.Context(), "all", 0, deps); err == nil {
			t.Fatal("invalid queue accepted")
		}
	})
}
