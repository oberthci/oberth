package service

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

func TestDirectCreateRealReceiveReleasesLockBeforePreparation(t *testing.T) {
	f := newGroupFixture(t, 0)
	source := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-b", "main")
	git("commit", "--allow-empty", "-m", "first")
	firstSHA := git("rev-parse", "HEAD")
	git("commit", "--allow-empty", "-m", "second")
	secondSHA := git("rev-parse", "HEAD")
	cache, err := gitcache.New(gitcache.Config{Root: filepath.Join(t.TempDir(), "cache"), AllowLocalUpstreams: true, Upstream: func(string) (string, error) { return source, nil }, Env: map[string]string{"GORACE": "atexit_sleep_ms=0"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	preparation := &blockedPreparationGit{WorkspaceGit: f.scheduler.git, entered: make(chan string, 2), release: make(chan struct{})}
	f.scheduler.git = preparation
	release := sync.OnceFunc(func() { close(preparation.release) })
	defer release()
	var accepted []model.Run
	handler := gitcache.ReceiveHandlerFunc(func(ctx context.Context, event gitcache.ReceiveEvent) error {
		result, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: event.ID, Repository: f.terraform, Branch: "feature/direct-receive", SHA: event.Update.NewSHA, OldSHA: event.Update.OldSHA, Actor: event.Actor})
		if err == nil {
			accepted = append(accepted, result.Run)
		}
		return err
	})
	// Send Git's wire protocol to the real receive transaction. Both objects
	// already exist in the cache; the empty pack requires no object transport.
	receive := func(oldSHA, newSHA string) {
		t.Helper()
		command := fmt.Sprintf("%s %s refs/heads/feature/direct-receive\x00report-status\n", oldSHA, newSHA)
		var input bytes.Buffer
		fmt.Fprintf(&input, "%04x%s0000", len(command)+4, command)
		pack, err := hex.DecodeString("5041434b0000000200000000029d08823bd8a8eab510ad6ac75c823cfd3ed31e")
		if err != nil {
			t.Fatal(err)
		}
		input.Write(pack)
		ctx, cancel := context.WithTimeout(t.Context(), testStartTimeout(10*time.Second))
		defer cancel()
		var output bytes.Buffer
		if err := cache.Receive(ctx, "example", "owner", "", &input, io.Discard, &output, handler); err != nil {
			t.Fatalf("receive blocked on preparation: %v %s", err, output.String())
		}
	}
	receive(strings.Repeat("0", 40), firstSHA)
	receive(firstSHA, secondSHA)
	for range 2 {
		select {
		case <-preparation.entered:
		case <-time.After(testStartTimeout(5 * time.Second)):
			t.Fatal("second receive did not prepare independently")
		}
	}
	if len(accepted) != 2 {
		t.Fatalf("accepted %d refs", len(accepted))
	}
	release()
	f.awaitCreated(t, 2)
	for _, run := range accepted {
		f.engine.finish(t, run.ID)
		f.awaitTerminal(t, run.ID)
	}
}
