package sshserver

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"golang.org/x/crypto/ssh"
)

func TestSSHReadDuringBlockedRollbackUsesWholeFeedback(t *testing.T) {
	longRoot := filepath.Join(t.TempDir(), strings.Repeat("long-temp-root-", 4))
	if err := os.Mkdir(longRoot, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOTMPDIR", longRoot)
	t.Run("long-temp-root", checkSSHReadDuringBlockedRollback)
}

func checkSSHReadDuringBlockedRollback(t *testing.T) {
	logs := make(chan string, 16)
	e := newReviewEnvConfigured(t, func(c *gitcache.Config) {
		c.Log = func(reason, detail string) { logs <- reason + ": " + detail }
	})
	record, lock := blockedReadReceive(t, e)
	output, err := readReceiveOverSSH(t, e.c)
	want := "push refused; rollback is blocked. Ask the server operator to fix repository storage, then retry this push.\n"
	if err == nil || string(output) != want {
		t.Fatal("SSH reader received native details or incomplete guidance", err, string(output))
	}
	select {
	case log := <-logs:
		if !strings.Contains(log, e.f.Store()) || !strings.Contains(log, filepath.Base(lock)) {
			t.Fatal("native lock path was not logged", log)
		}
	default:
		t.Fatal("native rollback failure was not logged")
	}
	if err = os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err = e.c.AbandonReceive(t.Context(), gittest.Repository, record.Event.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = e.c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main"); err != nil {
		t.Fatal("repair left the reader blocked", err)
	}
}

func blockedReadReceive(t *testing.T, e *reviewEnv) (gitcache.Reservation, string) {
	t.Helper()
	e.f.Write(t, "reader.txt", []byte("source"))
	e.f.CommitFile(t, "reader")
	r := agent(e.f)
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, io.ErrClosedPipe }
	if res := e.push(t, r, remote, "HEAD:refs/heads/feature"); res.err == nil {
		t.Fatal("receive did not stay pending")
	}
	record, err := e.c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil {
		t.Fatal(err)
	}
	record.State = "rejected"
	data, err := json.Marshal(record)
	if err == nil {
		err = os.WriteFile(filepath.Join(e.f.Store(), "oberth-receive.json"), data, 0600)
	}
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(e.f.Store(), "refs", "heads", "feature.lock")
	if err = os.WriteFile(lock, []byte("another writer"), 0600); err != nil {
		t.Fatal(err)
	}
	return record, lock
}

func readReceiveOverSSH(t *testing.T, cache *gitcache.Cache) ([]byte, error) {
	t.Helper()
	host, key := signer(t), signer(t)
	server, err := New(Config{Signer: host, Now: func() time.Time { return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) }, Lookup: func(context.Context, ssh.PublicKey) (Identity, error) {
		return Identity{Actor: "reader", CanAccess: func(repo string) bool { return repo == gittest.Repository }}, nil
	}, Run: func(ctx context.Context, s Session) error {
		return cache.Upload(ctx, s.Command.Repository, gitcache.ServiceIO{Input: s.Input, Output: s.Output, Begin: s.Begin, End: s.End})
	}})
	if err != nil {
		t.Fatal(err)
	}
	left, right := sshtest.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.Handle(t.Context(), right) }()
	native, channels, requests, err := ssh.NewClientConn(left, "local:22", &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey())})
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(native, channels, requests)
	defer func() { _ = client.Close(); <-done }()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	session.Stdin = strings.NewReader("0000")
	return session.CombinedOutput("git-upload-pack '/origin/team/widget.git'")
}
