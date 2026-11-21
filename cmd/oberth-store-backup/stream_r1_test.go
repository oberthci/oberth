package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/publicerr"
)

func encryptedBytes(t *testing.T, c config, size int) {
	t.Helper()
	recipient, err := age.ParseX25519Recipient(c.recipient)
	must(t, err)
	file, err := os.OpenFile(c.output, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	must(t, err)
	w, err := age.Encrypt(file, recipient)
	must(t, err)
	_, err = w.Write(bytes.Repeat([]byte{'x'}, size))
	must(t, err)
	must(t, w.Close())
	must(t, file.Close())
}

func TestPlaintextBoundPrecedesArchiveParsing(t *testing.T) {
	for _, size := range []int{1 + (64 << 10), 2 + (64 << 10)} {
		c := cryptoFixture(t)
		encryptedBytes(t, c, size)
		c = recoveryConfig(c)
		c.options.MaxBytes = 1
		key, err := readIdentity(c.identity)
		must(t, err)
		err = authenticate(t.Context(), c, key, io.Discard)
		if size == 1+(64<<10) {
			must(t, err)
		} else {
			requireReason(t, err, archiveTooLarge)
		}
	}
}

func TestRestoreLimitAndReceiptFields(t *testing.T) {
	for _, mode := range []string{"restore", "verify"} {
		t.Run(mode, func(t *testing.T) {
			c := cryptoFixture(t)
			must(t, encrypt(t.Context(), c, io.Discard))
			c = recoveryConfig(c)
			c.mode = mode
			info, err := os.Stat(c.database)
			must(t, err)
			c.options.MaxBytes = info.Size() - 1
			requireReason(t, decrypt(t.Context(), c, io.Discard), configuredTooLarge)
			c.options.MaxBytes = 0
			c.recipient = "untrusted supplied recipient"
			var out bytes.Buffer
			must(t, decrypt(t.Context(), c, &out))
			var receipt receipt
			must(t, json.Unmarshal(out.Bytes(), &receipt))
			key, err := readIdentity(c.identity)
			must(t, err)
			if receipt.Recipient != key.Recipient().String() {
				t.Fatal("receipt lost authenticated recipient")
			}
		})
	}
}

func TestAlteredHeadersHaveAnHonestCause(t *testing.T) {
	for _, where := range []string{"recipient", "mac", "intro"} {
		c := cryptoFixture(t)
		must(t, encrypt(t.Context(), c, io.Discard))
		body, err := os.ReadFile(c.output)
		must(t, err)
		index := 60
		want := identityMismatch
		if where == "mac" {
			index = bytes.Index(body, []byte("\n--- ")) + 5
		}
		if where == "intro" {
			index = 0
			want = notBackup
		}
		if body[index] == 'A' {
			body[index] = 'B'
		} else {
			body[index] = 'A'
		}
		must(t, os.WriteFile(c.output, body, 0600))
		c = recoveryConfig(c)
		requireReason(t, decrypt(t.Context(), c, io.Discard), want)
	}
}

func TestStreamGuardsPreserveCancellationAndPublicCauses(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	w := &boundedWriter{ctx: ctx, writer: &out, remaining: 1}
	n, err := w.Write([]byte{1})
	if n != 0 || out.Len() != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled writer changed output", n, err)
	}
	r := contextReader{ctx: ctx, Reader: bytes.NewReader([]byte{1})}
	n, err = r.Read(make([]byte, 1))
	if n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled reader consumed input", n, err)
	}
	refusal := publicerr.New(publicerr.InvalidInput, "specific stream refusal", "Choose another archive.", nil)
	if err := authenticatedStreamError(refusal); !errors.Is(err, refusal) || err.Error() != refusal.Error() {
		t.Fatal("public stream cause replaced", err)
	}
}

type manualIdle struct {
	elapsed, due, limit time.Duration
	expire              func()
	stopped             bool
}

func (c *manualIdle) start(limit time.Duration, expire func()) (func(), func()) {
	c.limit = limit
	c.expire = expire
	c.due = c.elapsed + limit
	return func() { c.due = c.elapsed + c.limit }, func() { c.stopped = true }
}
func (c *manualIdle) advance(d time.Duration) {
	c.elapsed += d
	if !c.stopped && c.elapsed >= c.due {
		c.expire()
	}
}

func TestIdleLimitAllowsLongProgressAndStopsStalls(t *testing.T) {
	clock := &manualIdle{}
	ctx, touch, stop := idleContext(t.Context(), idleLimit, clock.start)
	var out bytes.Buffer
	w := &boundedWriter{ctx: ctx, writer: &out, remaining: 100, progress: touch}
	for range 12 {
		clock.advance(4 * time.Minute)
		_, err := w.Write([]byte{1})
		must(t, err)
	}
	if ctx.Err() != nil {
		t.Fatal("total runtime was bounded")
	}
	// Empty writes are not progress.
	clock.advance(4 * time.Minute)
	_, err := w.Write(nil)
	must(t, err)
	clock.advance(time.Minute)
	requireReason(t, context.Cause(ctx), idleStalled)
	stop()
	if !clock.stopped {
		t.Fatal("idle timer retained after completion")
	}
}

func TestInstallationMessagePreservesSchemaVersionCause(t *testing.T) {
	c := cryptoFixture(t)
	must(t, encrypt(t.Context(), c, io.Discard))
	c = recoveryConfig(c)
	c.options.Schema++
	err := decrypt(t.Context(), c, io.Discard)
	var public *publicerr.Error
	if !errors.As(err, &public) || public.Code() != publicerr.SchemaRefused || public.Reason() == recoveryError(foreignInstallation, nil).Reason() {
		t.Fatal("schema version mislabeled as foreign installation", err)
	}
}

func TestBackupReportsSnapshotWorkBeforeArchiveOutput(t *testing.T) {
	c := cryptoFixture(t)
	copied := false
	c.progress = func() {
		out, err := os.Stat(c.output)
		if err != nil || out.Size() >= 1024 {
			return
		}
		stages, _ := filepath.Glob(filepath.Join(filepath.Dir(c.database), ".oberth-state-*", "oberth.sqlite"))
		for _, path := range stages {
			if info, err := os.Stat(path); err == nil && info.Size() > 0 {
				copied = true
			}
		}
	}
	must(t, encrypt(t.Context(), c, io.Discard))
	if !copied {
		t.Fatal("snapshot copied pages without resetting the idle watchdog")
	}
}
