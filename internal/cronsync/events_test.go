package cronsync_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestIrrelevantRefsAndCanceledWaitDoNotWaitOrList(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f.admitHook = func(ctx context.Context, _ cronsync.Preparation) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	finished := make(chan error, 1)
	go func() { finished <- f.c.SyncAll(t.Context()) }()
	<-entered
	for _, refs := range [][]string{{"refs/heads/unrelated"}} {
		if err := f.c.RefsChanged(t.Context(), f.repo.Qualified, refs); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.c.SetPause(ctx, f.repo, "nightly"); !errors.Is(err, context.Canceled) {
		t.Fatal("lock wait ignored cancellation", err)
	}
	once.Do(func() { close(release) })
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestBlockedGitAndNativeCallsPreserveCanceledWork(t *testing.T) {
	for _, port := range []string{"git", "native"} {
		t.Run(port, func(t *testing.T) {
			f := newFixture(t)
			entered := make(chan struct{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			hook := func(ctx context.Context, verb, _ string) error {
				if verb == "resolve" || verb == "list" {
					close(entered)
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}
			if port == "git" {
				f.gitHook = hook
			} else {
				f.hook = hook
			}
			finished := make(chan error, 1)
			go func() { finished <- f.c.SyncAll(ctx) }()
			<-entered
			cancel()
			if err := <-finished; !errors.Is(err, context.Canceled) {
				t.Fatal("canceled work lost", err)
			}
		})
	}
}

func TestIrrelevantRefsSkipNativeIOAndAdmission(t *testing.T) {
	f := newFixture(t)
	f.sync()
	calls := len(f.calls)
	nativeReads := 0
	f.hook = func(context.Context, string, string) error { nativeReads++; return nil }
	for _, ref := range []string{"refs/heads/unrelated", "refs/notes/commits"} {
		if err := f.c.RefsChanged(t.Context(), f.repo.Qualified, []string{ref}); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.calls) != calls || nativeReads != 0 {
		t.Fatal("irrelevant refs reached native storage or admission", len(f.calls), nativeReads)
	}
}

// A source-consuming parent seals the tag set, so a tag change re-admits the
// repository's schedules; a repository that declares none is not touched.
func TestTagChangeReadmitsDeclaredSchedules(t *testing.T) {
	f := newFixture(t)
	f.sync()
	calls := len(f.calls)
	if err := f.c.RefsChanged(t.Context(), f.repo.Qualified, []string{"refs/tags/v1"}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != calls+1 {
		t.Fatal("tag change did not re-admit the schedule", len(f.calls)-calls)
	}
	plain := cronsync.Repository{ID: 8, Qualified: "origin/team/plain", DefaultBranch: "main"}
	f.mu.Lock()
	f.repos = append(f.repos, plain)
	f.mu.Unlock()
	head := gitport.Ref{Name: "refs/heads/main", ObjectSHA: strings.Repeat("d", 40), CommitSHA: strings.Repeat("d", 40)}
	f.git.PutRef(plain.Qualified, head)
	f.git.PutFile(gitport.Revision{Repository: plain.Qualified, Commit: head.CommitSHA}, gitport.File{Path: "README", Mode: 0100644, Blob: strings.Repeat("e", 40), Bytes: []byte("plain\n")})
	calls, nativeReads := len(f.calls), 0
	f.hook = func(context.Context, string, string) error { nativeReads++; return nil }
	if err := f.c.RefsChanged(t.Context(), plain.Qualified, []string{"refs/tags/v2"}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != calls || nativeReads != 0 {
		t.Fatal("tag change reached a repository without schedules", len(f.calls)-calls, nativeReads)
	}
}
