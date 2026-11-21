package gitcache_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReviewSecondPublicationReceiveFence(t *testing.T) {
	t.Parallel()
	t.Run("publication", func(t *testing.T) { runPublicationRace(t, "publication") })
	t.Run("receive", func(t *testing.T) { runPublicationRace(t, "receive") })
}
func runPublicationRace(t *testing.T, first string) {

	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Write(t, "old-result", []byte("old"))
	old := f.CommitFile(t, "old-result")
	f.Git(t, f.Store(), "fetch", f.Work, "refs/heads/main:refs/heads/topic")
	f.Write(t, "new-result", []byte("new"))
	next := f.CommitFile(t, "new-result")
	req := publicationFor(f, "refs/heads/topic", old, "")
	receive := func(ctx context.Context) error {
		return c.Receive(
			ctx,
			gitcache.ReceiveRequest{
				Repository:  gittest.Repository,
				Actor:       "a",
				Fingerprint: "f",
				Admission:   gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
				Input: receiveRefInput(
					t,
					f,
					gitcache.RefUpdate{Ref: "refs/heads/topic", OldSHA: old, NewSHA: next},
				),
				Output: io.Discard,
				Deliver: func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
					return nil, nil
				},
			},
		)
	}
	published, received := make(chan error, 1), make(chan error, 1)
	if first == "publication" {
		reviewFault(t, f, map[string]any{"args": []string{"push"}, "stop": true})
		go func() { _, err := c.Publish(t.Context(), req); published <- err }()
	} else {
		reviewFault(t, f, map[string]any{"args": []string{"receive-pack"}, "needle": "refs/heads/topic", "phase": "after"})
		go func() { received <- receive(t.Context()) }()
	}
	marker := reviewWait(t, filepath.Join(f.Root, "stopped"))
	pid, err := strconv.Atoi(strings.Split(string(marker), "\n")[0])
	if err != nil {
		t.Fatal(err)
	}
	if first == "publication" {
		phase, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		checkReceiveBusy(t, f, old, func() error { return receive(phase) })
		cancel()
	} else {
		go func() { _, err := c.Publish(t.Context(), req); published <- err }()
	}

	reviewDisarm(t, f)
	if err = resumeStopped(t, pid); err != nil {
		t.Fatal(err)
	}
	var receivedErr, publishedErr error
	if first == "publication" {
		publishedErr = <-published
		receivedErr = receive(t.Context())
	} else {
		receivedErr = <-received
		publishedErr = <-published
	}
	checkPublicationRace(
		t,
		raceResult{
			first:      first,
			fixture:    f,
			commit:     next,
			receiveErr: receivedErr,
			publishErr: publishedErr,
		},
	)

}

func resumeStopped(t *testing.T, pid int) error {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	timeout := time.After(10 * time.Second)
	for {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "State:\tT") {
			return syscall.Kill(pid, syscall.SIGCONT)
		}
		select {
		case <-tick.C:
		case <-timeout:
			t.Fatal("native fixture did not stop")
		}
	}
}

type raceResult struct {
	first                  string
	fixture                *gittest.Fixture
	commit                 string
	receiveErr, publishErr error
}

func checkPublicationRace(t *testing.T, result raceResult) {
	t.Helper()
	if err := result.receiveErr; err != nil {
		t.Fatal(err)
	}
	err := result.publishErr
	if result.first == "receive" && err == nil {
		t.Fatal("older build published after newer receive")
	}
	if result.first == "publication" && err != nil {
		t.Fatal("publication that won gate was refused", err)
	}
	if got := result.fixture.Git(t, result.fixture.Store(), "rev-parse", "refs/heads/topic"); got != result.commit {
		t.Fatal("new receive lost", got)
	}
	if result.first == "receive" {
		out, e := os.ReadFile(filepath.Join(result.fixture.Root, "commands.jsonl"))
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(out), `"push"`) {
			t.Fatal("stale publication reached native push")
		}
	}
}

func checkReceiveBusy(t *testing.T, f *gittest.Fixture, old string, receive func() error) {
	t.Helper()
	err := receive()
	if err == nil || !strings.Contains(err.Error(), "push is in progress") {
		t.Fatal("receive was not refused before mutation", err)
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/topic"); got != old {
		t.Fatal("receive moved a ref during publication", got)
	}
}
