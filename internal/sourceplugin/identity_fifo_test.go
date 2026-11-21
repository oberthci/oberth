package sourceplugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTokenFIFORefusesWithoutBlockingOrReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	identityOK(t, syscall.Mkfifo(path, 0600))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := readIdentity(path); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Fatal("FIFO must refuse before reading and name its configured path")
		}
	case <-ctx.Done():
		// Release a blocked open so a failing control leaves no worker behind.
		holder, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
		identityOK(t, err)
		defer func() { _ = holder.Close() }()
		<-done
		t.Fatal("FIFO open blocked instead of refusing")
	}
}
