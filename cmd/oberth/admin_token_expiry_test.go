package main

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestAdminTokenExpiryWatcherWarnsOnlyInsideTheWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		expiry *model.AdminTokenExpiry
		warn   bool
	}{
		{&model.AdminTokenExpiry{EarliestExpiresAt: time.Now().Add(10 * 24 * time.Hour), Warning: "an admin uplink token expires in 240h0m0s"}, true},
		{&model.AdminTokenExpiry{EarliestExpiresAt: time.Now().Add(60 * 24 * time.Hour)}, false},
		{nil, false},
	} {
		var output lockedBuffer
		ctx, cancel := context.WithCancel(t.Context())
		read := make(chan struct{}, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			watchAdminTokenExpiry(ctx, func(context.Context) (*model.AdminTokenExpiry, error) {
				select {
				case read <- struct{}{}:
				default:
				}
				return tc.expiry, nil
			}, log.New(&output, "", 0))
		}()
		<-read
		for deadline := time.Now().Add(5 * time.Second); tc.warn && time.Now().Before(deadline) && !strings.Contains(output.String(), "WARNING"); {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		<-done
		if got := strings.Contains(output.String(), "WARNING: an admin uplink token expires"); got != tc.warn {
			t.Fatalf("expiry %+v: warning=%t output=%q", tc.expiry, got, output.String())
		}
	}
}
