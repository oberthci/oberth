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
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

func cryptoFixture(t *testing.T) config {
	t.Helper()
	parent := t.TempDir()
	must(t, os.Chmod(parent, 0700))
	options := stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: store.Schema}
	c := config{mode: "backup", database: filepath.Join(parent, "state.sqlite"), output: filepath.Join(parent, "backup.age"), identity: filepath.Join(parent, "identity.txt"), options: options}
	state, err := store.Create(t.Context(), c.database, store.DefaultOptions(func() time.Time { return time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC) }))
	must(t, err)
	must(t, state.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, store.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
	must(t, state.Close())
	key, err := age.GenerateX25519Identity()
	must(t, err)
	c.recipient = key.Recipient().String()
	must(t, os.WriteFile(c.identity, []byte("# backup identity\n"+key.String()+"\n"), 0600))
	return c
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func recoveryConfig(c config) config {
	c.mode, c.input = "restore", c.output
	c.output = filepath.Join(filepath.Dir(c.input), "restored.sqlite")
	return c
}

func TestAgeRecoveryRoundTrip(t *testing.T) {
	c := cryptoFixture(t)
	var out bytes.Buffer
	if runCommand(t.Context(), configArgs(c), &out, os.Stderr) != 0 {
		t.Fatal("backup command failed")
	}
	var saved receipt
	must(t, json.Unmarshal(out.Bytes(), &saved))
	if saved.Encryption != encryption || saved.Recipient != c.recipient || saved.CipherSHA != digestFile(t, c.output) {
		t.Fatal("receipt binding lost", saved)
	}
	if info, err := os.Stat(c.output); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("output custody", err)
	}
	key, err := readIdentity(c.identity)
	must(t, err)
	file, err := os.Open(c.output)
	must(t, err)
	plain, err := age.Decrypt(file, key)
	must(t, err)
	backup, err := store.NewBackup(c.database)
	must(t, err)
	_, err = backup.Inspect(t.Context(), c.options, plain)
	must(t, err)
	must(t, file.Close())
	c = recoveryConfig(c)
	c.mode = "verify"
	out.Reset()
	if runCommand(t.Context(), configArgs(c), &out, os.Stderr) != 0 {
		t.Fatal("verify command failed")
	}
	c.mode = "restore"
	out.Reset()
	if runCommand(t.Context(), configArgs(c), &out, os.Stderr) != 0 {
		t.Fatal("restore command failed")
	}
	state, err := store.Open(t.Context(), c.output, store.DefaultOptions(func() time.Time { return time.Time{} }))
	must(t, err)
	upstream, err := state.Upstream(t.Context(), "origin")
	must(t, err)
	if upstream.Org != "team" {
		t.Fatal("restored content lost")
	}
	must(t, state.Close())
	before := digestFile(t, c.output)
	if err := decrypt(t.Context(), c, io.Discard); publicerr.Classify(err).Code() != publicerr.Conflict {
		t.Fatal("existing destination accepted", err)
	}
	if digestFile(t, c.output) != before {
		t.Fatal("existing database changed")
	}
}

func TestStreamAndReceiptFailuresPreserveCauses(t *testing.T) {
	cause := errors.New("sink failed")
	writer := &boundedWriter{ctx: t.Context(), writer: failedWriter{cause}, remaining: 1}
	if _, err := writer.Write([]byte{1}); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte{1, 2}); err == nil {
		t.Fatal("output bound ignored")
	}
	var sink bytes.Buffer
	bounded := &boundedWriter{ctx: t.Context(), writer: &sink, remaining: 1}
	_, err := bounded.Write([]byte{1})
	must(t, err)
	if _, err := bounded.Write([]byte{2}); err == nil || sink.Len() != 1 {
		t.Fatal("stream exceeded its exact bound")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := emitReceipt(ctx, io.Discard, receipt{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := emitReceipt(t.Context(), failedWriter{cause}, receipt{}); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	c := cryptoFixture(t)
	if err := encrypt(t.Context(), c, failedWriter{cause}); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed receipt kept output", err)
	}
	if err := encrypt(ctx, c, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type failedWriter struct{ cause error }

func (w failedWriter) Write([]byte) (int, error) { return 0, w.cause }
