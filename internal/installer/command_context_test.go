package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultRunCommandRetainsContextCauseAfterChildStarted(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(t.Context(), time.Second)
			} else {
				ctx, cancel = context.WithCancel(t.Context())
			}
			defer cancel()
			marker := filepath.Join(t.TempDir(), "started")
			finished := make(chan error, 1)
			go func() {
				_, err := DefaultRunCommand(ctx, nil, "/bin/sh", "-c", `printf ready > "$1"; exec sleep 60`, "fixture", marker)
				finished <- err
			}()
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				select {
				case err := <-finished:
					t.Fatalf("child exited before ready: %v", err)
				case <-time.After(time.Millisecond):
				}
			}
			if mode == "cancel" {
				cancel()
			}
			err := <-finished
			want := context.Canceled
			if mode == "deadline" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatalf("lost %s cause: %v", mode, err)
			}
		})
	}
}
