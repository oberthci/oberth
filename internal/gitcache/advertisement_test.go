package gitcache_test

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReceiveAdvertisementNeverNamesKeepRefs(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	if err := c.Keep(t.Context(), "hidden", f.Bindings(f.Commit)); err != nil {
		t.Fatal(err)
	}
	input, send := io.Pipe()
	output, receive := io.Pipe()
	defer func() { _ = send.Close(); _ = output.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	r := gateReceive(f)
	r.Input = input
	r.Output = receive
	done := make(chan error, 1)
	go func() { err := c.Receive(ctx, r); _ = receive.CloseWithError(err); done <- err }()
	var advertised bytes.Buffer
	for {
		header := make([]byte, 4)
		if _, err := io.ReadFull(output, header); err != nil {
			t.Fatal(err)
		}
		n, err := strconv.ParseInt(string(header), 16, 32)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if n < 4 {
			t.Fatal("invalid packet")
		}
		if _, err = io.CopyN(&advertised, output, n-4); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(advertised.String(), "refs/oberth/") {
		t.Fatal("private retention advertised")
	}
	_, _ = io.WriteString(send, "0000")
	_ = send.Close()
	if err := <-done; err != nil {
		t.Fatal("empty handshake failed", err)
	}
}
