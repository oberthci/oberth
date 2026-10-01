package gitcache

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- Git's protocol is deliberately SHA-1 here.
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReceivePackAllowsOnlyBranchesAndTags(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	featureSHA := repository.featureCommit(t, "feature/ref-policy", "ref policy\n")
	receiveArgs := cache.serviceArgs(ReceivePack, ready.Path)
	receivePack := strings.Join(append([]string{cache.gitBinary}, receiveArgs[:len(receiveArgs)-1]...), " ")

	if output, err := pushWithReceivePack(repository.work, ready.Path, receivePack, featureSHA+":refs/heads/feature/ref-policy"); err != nil {
		t.Fatalf("push branch: %v\n%s", err, output)
	}
	if output, err := pushWithReceivePack(repository.work, ready.Path, receivePack, featureSHA+":refs/heads/main"); err != nil {
		t.Fatalf("push default branch: %v\n%s", err, output)
	}
	if got := runGit(t, ready.Path, "rev-parse", "refs/heads/main"); got != featureSHA {
		t.Fatalf("default branch moved to %s, want %s", got, featureSHA)
	}
	for name, ref := range map[string]string{
		"invalid branch": "refs/heads/refs/bad",
		"invalid tag":    "refs/tags/-bad",
		"overlong tag":   "refs/tags/" + strings.Repeat("a", maximumRefPartBytes+1),
	} {
		output, pushErr := pushWithReceivePack(repository.work, ready.Path, receivePack, featureSHA+":"+ref)
		if pushErr == nil || !strings.Contains(output, "invalid") {
			t.Fatalf("%s = %v\n%s; want receive-policy rejection", name, pushErr, output)
		}
		if got := runGitOptional(t, ready.Path, "show-ref", "--verify", ref); got != "" {
			t.Fatalf("%s created forbidden ref %s at %s", name, ref, got)
		}
	}
	for _, ref := range []string{
		"refs/replace/" + repository.initialSHA,
		"refs/notes/adversarial",
		"refs/remotes/origin/adversarial",
		"refs/oberth/forged",
		"refs/meta/config",
	} {
		output, err := pushWithReceivePack(repository.work, ready.Path, receivePack, featureSHA+":"+ref)
		if err == nil || !strings.Contains(output, "deny updating a hidden ref") {
			t.Fatalf("push to %s = %v\n%s; want hidden-ref rejection", ref, err, output)
		}
		if got := runGitOptional(t, ready.Path, "show-ref", "--verify", ref); got != "" {
			t.Fatalf("forbidden ref %s was created: %s", ref, got)
		}
	}
	output, err := rawReceiveUpdate(cache, ready.Path, featureSHA, repository.initialSHA, "HEAD")
	if !strings.Contains(output, "deny updating a hidden ref") {
		t.Fatalf("raw HEAD update = %v\n%q; want hidden-ref rejection", err, output)
	}
	if got := runGit(t, ready.Path, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Fatalf("HEAD changed to %q", got)
	}
}

func TestReceivePackRejectsNonFastForwardDefaultBranch(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	receiveArgs := cache.serviceArgs(ReceivePack, ready.Path)
	receivePack := strings.Join(append([]string{cache.gitBinary}, receiveArgs[:len(receiveArgs)-1]...), " ")

	// A fast-forward push to main must succeed.
	ffSHA := repository.featureCommit(t, "feature/ff-main", "ff update\n")
	if output, pushErr := pushWithReceivePack(repository.work, ready.Path, receivePack, ffSHA+":refs/heads/main"); pushErr != nil {
		t.Fatalf("fast-forward main push rejected: %v\n%s", pushErr, output)
	}
	if got := runGit(t, ready.Path, "rev-parse", "refs/heads/main"); got != ffSHA {
		t.Fatalf("main after FF = %s, want %s", got, ffSHA)
	}

	// A non-fast-forward (rebase) push to main must be rejected. Create a
	// divergent commit from the original initial commit, which is not an
	// ancestor of the current main (ffSHA).
	rebasedSHA := repository.featureCommit(t, "feature/rebased-main", "rebased content\n")
	output, pushErr := pushWithReceivePack(repository.work, ready.Path, receivePack, "+"+rebasedSHA+":refs/heads/main")
	if pushErr == nil {
		t.Fatalf("non-fast-forward main push accepted; want rejection\n%s", output)
	}
	if !strings.Contains(output, "non-fast-forward push to default branch") {
		t.Fatalf("rejection reason missing; got:\n%s", output)
	}
	// Main must not have moved.
	if got := runGit(t, ready.Path, "rev-parse", "refs/heads/main"); got != ffSHA {
		t.Fatalf("main moved after rejected non-FF push: %s, want %s", got, ffSHA)
	}

	// A force-push to a feature branch must still succeed.
	featureA := repository.featureCommit(t, "feature/forceable-a", "feature a\n")
	if output, pushErr := pushWithReceivePack(repository.work, ready.Path, receivePack, featureA+":refs/heads/feature/forceable"); pushErr != nil {
		t.Fatalf("create feature branch: %v\n%s", pushErr, output)
	}
	featureB := repository.featureCommit(t, "feature/forceable-b", "feature b\n")
	if output, pushErr := pushWithReceivePack(repository.work, ready.Path, receivePack, "+"+featureB+":refs/heads/feature/forceable"); pushErr != nil {
		t.Fatalf("force-push feature branch rejected: %v\n%s", pushErr, output)
	}
	if got := runGit(t, ready.Path, "rev-parse", "refs/heads/feature/forceable"); got != featureB {
		t.Fatalf("feature after force-push = %s, want %s", got, featureB)
	}
}

func TestReceivePackRejectsNonFastForwardCustomDefaultBranch(t *testing.T) {
	t.Parallel()
	repository := newTestRepositoryWithBranch(t, "trunk")
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	if ready.DefaultBranch != "trunk" {
		t.Fatalf("default branch = %q, want trunk", ready.DefaultBranch)
	}
	receiveArgs := cache.serviceArgs(ReceivePack, ready.Path)
	receivePack := strings.Join(append([]string{cache.gitBinary}, receiveArgs[:len(receiveArgs)-1]...), " ")

	// Fast-forward push to trunk succeeds.
	ffSHA := repository.featureCommit(t, "feature/ff-trunk", "ff trunk\n")
	if output, pushErr := pushWithReceivePack(repository.work, ready.Path, receivePack, ffSHA+":refs/heads/trunk"); pushErr != nil {
		t.Fatalf("fast-forward trunk push rejected: %v\n%s", pushErr, output)
	}

	// Non-fast-forward push to trunk is rejected.
	rebasedSHA := repository.featureCommit(t, "feature/rebased-trunk", "rebased trunk\n")
	output, pushErr := pushWithReceivePack(repository.work, ready.Path, receivePack, "+"+rebasedSHA+":refs/heads/trunk")
	if pushErr == nil {
		t.Fatalf("non-fast-forward trunk push accepted; want rejection\n%s", output)
	}
	if !strings.Contains(output, "non-fast-forward push to default branch") {
		t.Fatalf("rejection reason missing; got:\n%s", output)
	}
}

func TestReceivePackRejectsUnreachableMovedAndDeletedReleaseTagsBeforeRefMutation(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	lock := cache.repoLock("example")
	lock.Lock()
	_, err = cache.prepareReleaseAdmissionLocked(context.Background(), ready.Path, true)
	lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	receiveArgs := cache.serviceArgs(ReceivePack, ready.Path)
	receivePack := strings.Join(append([]string{cache.gitBinary}, receiveArgs[:len(receiveArgs)-1]...), " ")

	// A tag can legally share the default branch's short name. Admission must
	// derive the branch from full symbolic HEAD, not Git's ambiguous --short
	// rendering (which becomes "heads/main" in this case).
	const collidingTagRef = "refs/tags/main"
	if output, pushErr := pushWithReceivePack(repository.work, ready.Path, receivePack, repository.initialSHA+":"+collidingTagRef); pushErr != nil {
		t.Fatalf("create tag matching default branch: %v\n%s", pushErr, output)
	}

	const releaseRef = "refs/tags/v1.0.0"
	if output, pushErr := pushWithReceivePack(repository.work, ready.Path, receivePack, repository.initialSHA+":"+releaseRef); pushErr != nil {
		t.Fatalf("create reachable tag: %v\n%s", pushErr, output)
	}
	if got := runGit(t, ready.Path, "rev-parse", releaseRef); got != repository.initialSHA {
		t.Fatalf("created tag = %s, want %s", got, repository.initialSHA)
	}

	reachableReplacement := repository.commit(t, "reachable replacement", "reachable replacement\n")
	if _, err := cache.Ensure(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	lock.Lock()
	_, err = cache.prepareReleaseAdmissionLocked(context.Background(), ready.Path, true)
	lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]string{
		"move":   "+" + reachableReplacement + ":" + releaseRef,
		"delete": ":" + releaseRef,
	} {
		output, pushErr := pushWithReceivePack(repository.work, ready.Path, receivePack, spec)
		if pushErr == nil || !strings.Contains(output, "release tags are immutable") {
			t.Fatalf("%s immutable tag = %v\n%s", name, pushErr, output)
		}
		if got := runGit(t, ready.Path, "rev-parse", releaseRef); got != repository.initialSHA {
			t.Fatalf("tag after rejected %s = %s, want %s", name, got, repository.initialSHA)
		}
	}

	unreachable := repository.featureCommit(t, "feature/unreachable-tag", "unreachable\n")
	const unreachableRef = "refs/tags/v2.0.0"
	output, pushErr := pushWithReceivePack(repository.work, ready.Path, receivePack, unreachable+":"+unreachableRef)
	if pushErr == nil || !strings.Contains(output, "not reachable") {
		t.Fatalf("unreachable tag = %v\n%s", pushErr, output)
	}
	if got := runGitOptional(t, ready.Path, "show-ref", "--verify", unreachableRef); got != "" {
		t.Fatalf("rejected unreachable tag became public: %s", got)
	}
}

func TestInvalidUpstreamRefsNeverPoisonReceiveOrRestart(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	for _, ref := range []string{"refs/heads/refs/bad", "refs/tags/-bad"} {
		runGit(t, repository.work, "push", "origin", repository.initialSHA+":"+ref)
	}
	root := filepath.Join(t.TempDir(), "cache")
	cache := newTestCacheAt(t, root, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"refs/heads/refs/bad", "refs/tags/-bad"} {
		if got := runGitOptional(t, ready.Path, "show-ref", "--verify", ref); got != "" {
			t.Fatalf("invalid upstream ref %s was materialized at %s", ref, got)
		}
	}

	// Simulate a public ref materialized by an older binary. Ensure must remove
	// it before any receive snapshots public state.
	const legacyRef = "refs/tags/-legacy"
	lock := cache.repoLock("example")
	lock.Lock()
	err = cache.updateRef(context.Background(), ready.Path, legacyRef, repository.initialSHA, "")
	lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Ensure(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	if got := runGitOptional(t, ready.Path, "show-ref", "--verify", legacyRef); got != "" {
		t.Fatalf("legacy invalid public ref survived sanitation at %s", got)
	}

	featureSHA := repository.featureCommit(t, "feature/valid-after-invalid-upstream", "valid receive\n")
	seedCacheObject(t, repository.work, ready.Path, featureSHA)
	callbackFailure := errors.New("simulate process loss before callback checkpoint")
	err = cache.receiveTransaction(context.Background(), "example", ready.Path, "valid-actor", func() error {
		return cache.updateRef(context.Background(), ready.Path, "refs/heads/feature/valid-after-invalid-upstream", featureSHA, "")
	}, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return callbackFailure }))
	if !errors.Is(err, callbackFailure) {
		t.Fatalf("valid receive callback failure = %v", err)
	}

	restarted := newTestCacheAt(t, root, repository.upstream)
	var replayed ReceiveEvent
	if err := restarted.ReplayPending(context.Background(), ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		replayed = event
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if replayed.Actor != "valid-actor" || replayed.Update.Ref != "refs/heads/feature/valid-after-invalid-upstream" || replayed.Update.NewSHA != featureSHA {
		t.Fatalf("replayed valid event = %#v", replayed)
	}
	if pending, err := restarted.PendingReceives(); err != nil || len(pending) != 0 {
		t.Fatalf("pending receives after restart = %#v, %v", pending, err)
	}
}

func TestGitCommandsDisableAndCleanReplacementObjects(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	replacement := repository.commit(t, "replacement subject", "replacement\n")
	seedCacheObject(t, repository.work, ready.Path, replacement)
	runGit(t, ready.Path, "replace", repository.initialSHA, replacement)

	subject, err := cache.capture(context.Background(), ready.Path, "show", "-s", "--format=%s", repository.initialSHA)
	if err != nil {
		t.Fatal(err)
	}
	if subject != "initial" {
		t.Fatalf("server-side Git honored replacement object: subject %q", subject)
	}
	if _, err := cache.Ensure(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	if got := runGitOptional(t, ready.Path, "for-each-ref", "--format=%(refname)", "refs/replace/"); got != "" {
		t.Fatalf("replacement refs survived cache sanitation: %s", got)
	}
}

func TestReceiveTransactionSerializesActorBoundSnapshots(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	shaA := repository.featureCommit(t, "feature/actor-a", "actor a\n")
	shaB := repository.featureCommit(t, "feature/actor-b", "actor b\n")
	seedCacheObject(t, repository.work, ready.Path, shaA)
	seedCacheObject(t, repository.work, ready.Path, shaB)
	ref := "refs/heads/shared"
	firstPublished := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{}, 1)
	secondAttempted := make(chan struct{})

	type observed struct {
		actor string
		sha   string
	}
	var observedMu sync.Mutex
	var events []observed
	handler := ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		observedMu.Lock()
		events = append(events, observed{actor: event.Actor, sha: event.Update.NewSHA})
		observedMu.Unlock()
		return nil
	})

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- cache.receiveTransaction(context.Background(), "example", ready.Path, "actor-a", func() error {
			if err := cache.updateRef(context.Background(), ready.Path, ref, shaA, ""); err != nil {
				return err
			}
			close(firstPublished)
			<-releaseFirst
			return nil
		}, handler)
	}()
	<-firstPublished

	secondDone := make(chan error, 1)
	go func() {
		close(secondAttempted)
		secondDone <- cache.receiveTransaction(context.Background(), "example", ready.Path, "actor-b", func() error {
			secondEntered <- struct{}{}
			return cache.updateRef(context.Background(), ready.Path, ref, shaB, shaA)
		}, handler)
	}()
	<-secondAttempted
	select {
	case <-secondEntered:
		t.Fatal("second actor entered receive transaction before first capture completed")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}

	observedMu.Lock()
	defer observedMu.Unlock()
	if len(events) != 2 || events[0] != (observed{actor: "actor-a", sha: shaA}) || events[1] != (observed{actor: "actor-b", sha: shaB}) {
		t.Fatalf("actor-bound events = %+v", events)
	}
}

func TestReceiveCallbackCanInspectAcceptedGitObject(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	sha := repository.featureCommit(t, "feature/callback-reentry", "callback reentry\n")
	seedCacheObject(t, repository.work, ready.Path, sha)

	done := make(chan error, 1)
	go func() {
		done <- cache.receiveTransaction(context.Background(), "example", ready.Path, "actor", func() error {
			return cache.updateRef(context.Background(), ready.Path, "refs/heads/feature/callback-reentry", sha, "")
		}, ReceiveHandlerFunc(func(ctx context.Context, event ReceiveEvent) error {
			// The production handler follows this same path: repository discovery
			// refreshes the cache, then branch/tag admission peels the accepted
			// object. Neither call may deadlock on the receive transaction lock.
			if _, err := cache.Ensure(ctx, event.Repo); err != nil {
				return err
			}
			peeled, err := cache.PeelObject(ctx, event.Repo, event.Update.NewSHA)
			if err != nil {
				return err
			}
			if peeled.ObjectSHA != sha || peeled.CommitSHA != sha {
				return fmt.Errorf("peeled object = %+v, want %s", peeled, sha)
			}
			return nil
		}))
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("receive callback deadlocked while re-entering Git cache")
	}
}

func TestReceiveCallbackDeliveryKeepsLaterReceivesOrdered(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	firstSHA := repository.featureCommit(t, "feature/ordered-first", "ordered first\n")
	secondSHA := repository.featureCommit(t, "feature/ordered-second", "ordered second\n")
	seedCacheObject(t, repository.work, ready.Path, firstSHA)
	seedCacheObject(t, repository.work, ready.Path, secondSHA)

	callbackEntered := make(chan struct{})
	releaseCallback := make(chan struct{})
	secondReceiveEntered := make(chan struct{}, 1)
	handler := ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		if event.Actor == "first-actor" {
			close(callbackEntered)
			<-releaseCallback
		}
		return nil
	})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- cache.receiveTransaction(context.Background(), "example", ready.Path, "first-actor", func() error {
			return cache.updateRef(context.Background(), ready.Path, "refs/heads/feature/ordered-first", firstSHA, "")
		}, handler)
	}()
	<-callbackEntered

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- cache.receiveTransaction(context.Background(), "example", ready.Path, "second-actor", func() error {
			secondReceiveEntered <- struct{}{}
			return cache.updateRef(context.Background(), ready.Path, "refs/heads/feature/ordered-second", secondSHA, "")
		}, handler)
	}()
	select {
	case <-secondReceiveEntered:
		t.Fatal("later receive began before the prior durable callback completed")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseCallback)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestReceiveOutboxRetriesAndReplaysAfterRestart(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	cache := newTestCacheAt(t, root, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	sha := repository.featureCommit(t, "feature/replay", "replay\n")
	seedCacheObject(t, repository.work, ready.Path, sha)
	ref := "refs/heads/feature/replay"
	callbackFailure := errors.New("database unavailable")
	var failedEvent ReceiveEvent
	err = cache.receiveTransaction(context.Background(), "example", ready.Path, "actor-original", func() error {
		return cache.updateRef(context.Background(), ready.Path, ref, sha, "")
	}, ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		failedEvent = event
		return callbackFailure
	}))
	if !errors.Is(err, callbackFailure) || failedEvent.ID == "" {
		t.Fatalf("receive error/event = %v, %+v", err, failedEvent)
	}

	restarted := newTestCacheAt(t, root, repository.upstream)
	var replayed []ReceiveEvent
	if err := restarted.ReplayPending(context.Background(), ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		replayed = append(replayed, event)
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 || replayed[0].ID != failedEvent.ID || replayed[0].Actor != "actor-original" || replayed[0].Update.Ref != ref {
		t.Fatalf("replayed events = %+v, want stable original %+v", replayed, failedEvent)
	}
	if pending, err := restarted.PendingReceives(); err != nil || len(pending) != 0 {
		t.Fatalf("pending after replay = %+v, %v", pending, err)
	}
}

func TestReleaseReplayKeepsExactAdmissionAcrossDefaultBranchRename(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	cache := newTestCacheAt(t, root, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	repo, path, err := cache.path("example")
	if err != nil {
		t.Fatal(err)
	}
	lock := cache.repoLock(repo)
	lock.Lock()
	admission, err := cache.prepareReleaseAdmissionLocked(context.Background(), ready.Path, true)
	if err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	if _, err := cache.reserveReceiveLocked(context.Background(), repo, path, "release-actor", admission); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	const releaseRef = "refs/tags/v1.0.0-replay"
	if err := cache.updateRef(context.Background(), path, releaseRef, repository.initialSHA, ""); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock() // Simulate process loss after the accepted tag ref was published.

	// Upstream renames its default branch before the durable callback replays.
	// The handler refresh below must not switch release ancestry to that later
	// name or depend on a hidden admission ref that this receive never created.
	runGit(t, repository.work, "branch", "-m", "trunk")
	runGit(t, repository.work, "push", "origin", "trunk")
	runGit(t, repository.upstream, "symbolic-ref", "HEAD", "refs/heads/trunk")

	restarted := newTestCacheAt(t, root, repository.upstream)
	var replayed ReceiveEvent
	if err := restarted.ReplayPending(context.Background(), ReceiveHandlerFunc(func(ctx context.Context, event ReceiveEvent) error {
		refreshed, err := restarted.Ensure(ctx, event.Repo)
		if err != nil {
			return err
		}
		if refreshed.DefaultBranch != "trunk" {
			return fmt.Errorf("refreshed default branch = %q, want trunk", refreshed.DefaultBranch)
		}
		if event.ReleaseAdmission != admission {
			return fmt.Errorf("release admission = %#v, want %#v", event.ReleaseAdmission, admission)
		}
		reachable, err := restarted.ReleaseReachable(ctx, event.Repo, event.Update.NewSHA, event.ReleaseAdmission.SHA)
		if err != nil {
			return err
		}
		if !reachable {
			return errors.New("accepted release lost its original ancestry proof")
		}
		replayed = event
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if replayed.Update.Ref != releaseRef || replayed.Actor != "release-actor" || replayed.ReleaseAdmission.DefaultBranch != "main" {
		t.Fatalf("replayed release event = %#v", replayed)
	}
	if pending, err := restarted.PendingReceives(); err != nil || len(pending) != 0 {
		t.Fatalf("pending release receives = %#v, %v", pending, err)
	}
}

func TestLaterReceiveReplaysEarlierFailureBeforeAcceptingNewActor(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	firstSHA := repository.featureCommit(t, "feature/first", "first\n")
	secondSHA := repository.featureCommit(t, "feature/second", "second\n")
	seedCacheObject(t, repository.work, ready.Path, firstSHA)
	seedCacheObject(t, repository.work, ready.Path, secondSHA)
	callbackFailure := errors.New("temporary callback failure")
	if err := cache.receiveTransaction(context.Background(), "example", ready.Path, "first-actor", func() error {
		return cache.updateRef(context.Background(), ready.Path, "refs/heads/feature/first", firstSHA, "")
	}, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return callbackFailure })); !errors.Is(err, callbackFailure) {
		t.Fatalf("first receive error = %v", err)
	}

	var events []ReceiveEvent
	err = cache.receiveTransaction(context.Background(), "example", ready.Path, "second-actor", func() error {
		if len(events) != 1 || events[0].Actor != "first-actor" {
			return fmt.Errorf("new receive began before prior replay: %+v", events)
		}
		return cache.updateRef(context.Background(), ready.Path, "refs/heads/feature/second", secondSHA, "")
	}, ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		events = append(events, event)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Actor != "first-actor" || events[1].Actor != "second-actor" {
		t.Fatalf("receive replay order = %+v", events)
	}
}

func TestStartupReplayRecoversReservationPublishedBeforeFinalize(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	cache := newTestCacheAt(t, root, repository.upstream)
	_, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	sha := repository.featureCommit(t, "feature/crash-window", "crash window\n")
	repo, path, err := cache.path("example")
	if err != nil {
		t.Fatal(err)
	}
	seedCacheObject(t, repository.work, path, sha)
	lock := cache.repoLock(repo)
	lock.Lock()
	if _, err := cache.reserveReceiveLocked(context.Background(), repo, path, "actor-before-crash", ReleaseAdmission{}); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	if err := cache.updateRef(context.Background(), path, "refs/heads/feature/crash-window", sha, ""); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock() // Simulate process loss after receive-pack published the ref.

	restarted := newTestCacheAt(t, root, repository.upstream)
	var got ReceiveEvent
	if err := restarted.ReplayPending(context.Background(), ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		got = event
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if got.Actor != "actor-before-crash" || got.Update.NewSHA != sha || got.Update.Ref != "refs/heads/feature/crash-window" {
		t.Fatalf("recovered event = %+v", got)
	}
}

func TestReceiveReplaysReservedCrashBeforeRefreshingUpstream(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	cache := newTestCacheAt(t, root, repository.upstream)
	_, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	repo, path, err := cache.path("example")
	if err != nil {
		t.Fatal(err)
	}
	featureSHA := repository.featureCommit(t, "feature/before-crash", "before crash\n")
	seedCacheObject(t, repository.work, path, featureSHA)
	lock := cache.repoLock(repo)
	lock.Lock()
	if _, err := cache.reserveReceiveLocked(context.Background(), repo, path, "actor-before-crash", ReleaseAdmission{}); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	if err := cache.updateRef(context.Background(), path, "refs/heads/feature/before-crash", featureSHA, ""); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock()

	// Upstream moves while this process is down. If Receive refreshes before it
	// recovers the reserved snapshot, that unrelated movement is attributed to
	// actor-before-crash.
	repository.checkout(t, "main")
	upstreamSHA := repository.commit(t, "upstream moved", "upstream moved\n")

	restarted := newTestCacheAt(t, root, repository.upstream)
	var events []ReceiveEvent
	err = restarted.Receive(context.Background(), "example", "next-actor", "", strings.NewReader(""), io.Discard, io.Discard,
		ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
			events = append(events, event)
			return nil
		}))
	if err == nil {
		t.Fatal("empty receive-pack input unexpectedly succeeded")
	}
	if len(events) != 1 || events[0].Actor != "actor-before-crash" || events[0].Update.Ref != "refs/heads/feature/before-crash" {
		t.Fatalf("recovered events = %+v; upstream movement was attributed to the crashed receive", events)
	}
	if got := runGit(t, path, "rev-parse", "refs/heads/main"); got != upstreamSHA {
		t.Fatalf("cache main = %s, want refreshed upstream %s", got, upstreamSHA)
	}
}

func pushWithReceivePack(dir, remote, receivePack, refspec string) (string, error) {
	command := exec.Command("git", "push", "--receive-pack="+receivePack, remote, refspec)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	return string(output), err
}

func rawReceiveUpdate(cache *Cache, path, oldSHA, newSHA, ref string) (string, error) {
	command := fmt.Sprintf("%s %s %s\x00report-status\n", oldSHA, newSHA, ref)
	var input bytes.Buffer
	_, _ = fmt.Fprintf(&input, "%04x%s0000", len(command)+4, command)
	emptyPack := []byte{'P', 'A', 'C', 'K', 0, 0, 0, 2, 0, 0, 0, 0}
	checksum := sha1.Sum(emptyPack)
	_, _ = input.Write(emptyPack)
	_, _ = input.Write(checksum[:])
	var output bytes.Buffer
	err := cache.run(context.Background(), commandSpec{
		dir: path, args: cache.serviceArgs(ReceivePack, path), stdin: &input, stdout: &output, stderr: &output,
	})
	return output.String(), err
}

func runGitOptional(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func seedCacheObject(t *testing.T, work, cachePath, sha string) {
	t.Helper()
	runGit(t, work, "push", cachePath, sha+":refs/oberth/test/"+sha)
}

// TestPreFinalizeGateBlocksRefPublication proves that a failing
// PreFinalizeGate prevents refs from being finalized into the durable
// outbox. The receive function itself (which updates Git refs as a side
// effect) runs first; the gate fires between that and the finalization
// that would make those ref changes visible to callbacks.
func TestPreFinalizeGateBlocksRefPublication(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache, err := New(Config{
		Root:           filepath.Join(t.TempDir(), "cache"),
		CommandTimeout: 10 * time.Second,
		Upstream: func(repo string) (string, error) {
			if repo != "example" {
				return "", fmt.Errorf("unknown repo %s", repo)
			}
			return repository.upstream, nil
		},
		PreFinalizeGate: func(context.Context) error {
			return errors.New("audit chain tampered during pack upload")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	featureSHA := repository.featureCommit(t, "feature/gated", "gated content\n")
	seedCacheObject(t, repository.work, ready.Path, featureSHA)

	var handlerCalled bool
	handler := ReceiveHandlerFunc(func(_ context.Context, _ ReceiveEvent) error {
		handlerCalled = true
		return nil
	})

	repo, path, pathErr := cache.path("example")
	if pathErr != nil {
		t.Fatal(pathErr)
	}

	// Use the internal receiveTransaction which takes a synthetic receive
	// function. The receive function simulates what receive-pack does:
	// it updates a ref in the bare cache.
	err = cache.receiveTransaction(context.Background(), repo, path, "gate-actor",
		func() error {
			return cache.updateRef(context.Background(), path, "refs/heads/feature/gated", featureSHA, "")
		}, handler)
	if err == nil {
		t.Fatal("receiveTransaction should have failed when the pre-finalize gate rejects")
	}
	if !strings.Contains(err.Error(), "audit chain tampered") {
		t.Fatalf("error = %v, want mention of audit chain tampered", err)
	}
	if handlerCalled {
		t.Fatal("handler was called despite the pre-finalize gate failing — refs were published")
	}
	// The reservation must persist in gate-failed state — removing it
	// (#28-8 round 2) caused record amnesia: the next receive re-baselined
	// the moved refs and lost the push permanently.
	reservation, readErr := cache.readReservation(repo)
	if readErr != nil {
		t.Fatalf("reservation should persist after gate failure: %v", readErr)
	}
	if reservation.State != receiveGateFailed {
		t.Fatalf("reservation state = %q, want %q", reservation.State, receiveGateFailed)
	}
	if reservation.Actor != "gate-actor" {
		t.Fatalf("reservation actor = %q, want gate-actor", reservation.Actor)
	}
	// Receive-pack applied the ref update; it is visible despite the gate failure.
	if got := runGitOptional(t, path, "show-ref", "--verify", "refs/heads/feature/gated"); got == "" {
		t.Fatal("ref should be visible after gate failure (receive-pack applied it)")
	}
}

// TestGateFailedReplayDeliversWithOriginalAttribution verifies that a
// gate-failed reservation recovers correctly when the gate becomes healthy:
// callbacks fire with the original actor and admission, and the reservation
// is consumed.
func TestGateFailedReplayDeliversWithOriginalAttribution(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	upstreamFunc := func(repo string) (string, error) {
		if repo != "example" {
			return "", fmt.Errorf("unknown repo %s", repo)
		}
		return repository.upstream, nil
	}
	cache, err := New(Config{
		Root:           root,
		CommandTimeout: 10 * time.Second,
		Upstream:       upstreamFunc,
		PreFinalizeGate: func(context.Context) error {
			return errors.New("gate failing")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	featureSHA := repository.featureCommit(t, "feature/gate-recover", "gate recover\n")
	seedCacheObject(t, repository.work, ready.Path, featureSHA)

	repo, path, pathErr := cache.path("example")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	err = cache.receiveTransaction(context.Background(), repo, path, "original-actor",
		func() error {
			return cache.updateRef(context.Background(), path, "refs/heads/feature/gate-recover", featureSHA, "")
		}, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error {
			t.Fatal("handler should not be called on gate failure")
			return nil
		}))
	if err == nil || !strings.Contains(err.Error(), "gate failing") {
		t.Fatalf("receive error = %v, want gate failing", err)
	}

	// Restart with a healthy gate — simulates gate recovery (e.g. TSA reachable again).
	restarted, err := New(Config{
		Root:           root,
		CommandTimeout: 10 * time.Second,
		Upstream:       upstreamFunc,
	})
	if err != nil {
		t.Fatal(err)
	}
	var replayed []ReceiveEvent
	if err := restarted.ReplayPending(context.Background(), ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		replayed = append(replayed, event)
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 {
		t.Fatalf("replayed %d events, want 1", len(replayed))
	}
	if replayed[0].Actor != "original-actor" {
		t.Fatalf("replayed actor = %q, want original-actor", replayed[0].Actor)
	}
	if replayed[0].Update.Ref != "refs/heads/feature/gate-recover" || replayed[0].Update.NewSHA != featureSHA {
		t.Fatalf("replayed event = %+v", replayed[0])
	}
	// Reservation consumed after successful replay.
	if pending, err := restarted.PendingReceives(); err != nil || len(pending) != 0 {
		t.Fatalf("pending after recovery = %+v, %v", pending, err)
	}
	// Ref still visible.
	if got := runGitOptional(t, path, "show-ref", "--verify", "refs/heads/feature/gate-recover"); got == "" {
		t.Fatal("ref should still be visible after gate recovery")
	}
}

// TestGateFailedReplayRefusesWhileGateStillFails verifies that replay of a
// gate-failed reservation returns an error and keeps the reservation when the
// gate is still failing — no amnesia, no bypass.
func TestGateFailedReplayRefusesWhileGateStillFails(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	gateErr := errors.New("persistent gate failure")
	upstreamFunc := func(repo string) (string, error) {
		if repo != "example" {
			return "", fmt.Errorf("unknown repo %s", repo)
		}
		return repository.upstream, nil
	}
	cache, err := New(Config{
		Root:           root,
		CommandTimeout: 10 * time.Second,
		Upstream:       upstreamFunc,
		PreFinalizeGate: func(context.Context) error {
			return gateErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	featureSHA := repository.featureCommit(t, "feature/gate-persist", "gate persist\n")
	seedCacheObject(t, repository.work, ready.Path, featureSHA)

	repo, path, pathErr := cache.path("example")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	err = cache.receiveTransaction(context.Background(), repo, path, "persist-actor",
		func() error {
			return cache.updateRef(context.Background(), path, "refs/heads/feature/gate-persist", featureSHA, "")
		}, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return nil }))
	if err == nil {
		t.Fatal("receive should fail on gate error")
	}

	// Restart with still-failing gate.
	restarted, err := New(Config{
		Root:           root,
		CommandTimeout: 10 * time.Second,
		Upstream:       upstreamFunc,
		PreFinalizeGate: func(context.Context) error {
			return gateErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReplayPending(context.Background(), ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error {
		t.Fatal("handler should not be called while gate still fails")
		return nil
	})); err == nil {
		t.Fatal("replay should fail while gate is still failing")
	}
	// Reservation must still exist in gate-failed state.
	reservation, readErr := restarted.readReservation(repo)
	if readErr != nil {
		t.Fatalf("reservation should persist after failed replay: %v", readErr)
	}
	if reservation.State != receiveGateFailed {
		t.Fatalf("reservation state = %q, want %q", reservation.State, receiveGateFailed)
	}
	if reservation.Actor != "persist-actor" {
		t.Fatalf("reservation actor = %q, want persist-actor", reservation.Actor)
	}
	// Ref still visible — receive-pack applied it.
	if got := runGitOptional(t, path, "show-ref", "--verify", "refs/heads/feature/gate-persist"); got == "" {
		t.Fatal("ref should be visible despite persistent gate failure")
	}
}

// TestGateFailedReservationRecoveredByLaterReceive verifies that a later
// receive replays the gate-failed reservation (re-running the gate) before
// accepting the new actor's push — closing both the gate-bypass and
// record-amnesia windows.
func TestGateFailedReservationRecoveredByLaterReceive(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	var gateCallCount int
	var gateMu sync.Mutex
	upstreamFunc := func(repo string) (string, error) {
		if repo != "example" {
			return "", fmt.Errorf("unknown repo %s", repo)
		}
		return repository.upstream, nil
	}
	cache, err := New(Config{
		Root:           root,
		CommandTimeout: 10 * time.Second,
		Upstream:       upstreamFunc,
		PreFinalizeGate: func(context.Context) error {
			gateMu.Lock()
			gateCallCount++
			n := gateCallCount
			gateMu.Unlock()
			if n == 1 {
				return errors.New("transient gate failure")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	firstSHA := repository.featureCommit(t, "feature/gate-first", "gate first\n")
	secondSHA := repository.featureCommit(t, "feature/gate-second", "gate second\n")
	seedCacheObject(t, repository.work, ready.Path, firstSHA)
	seedCacheObject(t, repository.work, ready.Path, secondSHA)

	repo, path, pathErr := cache.path("example")
	if pathErr != nil {
		t.Fatal(pathErr)
	}

	// First receive — gate fails.
	var events []ReceiveEvent
	handler := ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		events = append(events, event)
		return nil
	})
	err = cache.receiveTransaction(context.Background(), repo, path, "first-actor",
		func() error {
			return cache.updateRef(context.Background(), path, "refs/heads/feature/gate-first", firstSHA, "")
		}, handler)
	if err == nil || !strings.Contains(err.Error(), "transient gate failure") {
		t.Fatalf("first receive error = %v, want transient gate failure", err)
	}
	if len(events) != 0 {
		t.Fatalf("events after gate failure = %+v, want none", events)
	}

	// Second receive — gate passes (call count > 1). The replay of the
	// gate-failed first reservation must succeed before the second actor's
	// receive begins, and both events must be delivered in order.
	err = cache.receiveTransaction(context.Background(), repo, path, "second-actor",
		func() error {
			// The first actor's replay must have delivered before this runs.
			if len(events) != 1 || events[0].Actor != "first-actor" {
				return fmt.Errorf("second receive began before first replay: %+v", events)
			}
			return cache.updateRef(context.Background(), path, "refs/heads/feature/gate-second", secondSHA, "")
		}, handler)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Actor != "first-actor" || events[1].Actor != "second-actor" {
		t.Fatalf("events = %+v, want [first-actor, second-actor]", events)
	}
	if events[0].Update.Ref != "refs/heads/feature/gate-first" || events[0].Update.NewSHA != firstSHA {
		t.Fatalf("first event = %+v", events[0])
	}
	if events[1].Update.Ref != "refs/heads/feature/gate-second" || events[1].Update.NewSHA != secondSHA {
		t.Fatalf("second event = %+v", events[1])
	}
}

// TestGateFailedPendingReceiveShowsGateFailedState verifies that
// PendingReceives reports gate-failed reservations with the correct state
// and actor for diagnostic visibility.
func TestGateFailedPendingReceiveShowsGateFailedState(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	cache, err := New(Config{
		Root:           root,
		CommandTimeout: 10 * time.Second,
		Upstream: func(repo string) (string, error) {
			if repo != "example" {
				return "", fmt.Errorf("unknown repo %s", repo)
			}
			return repository.upstream, nil
		},
		PreFinalizeGate: func(context.Context) error {
			return errors.New("gate check failed")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	featureSHA := repository.featureCommit(t, "feature/pending-diag", "pending diag\n")
	seedCacheObject(t, repository.work, ready.Path, featureSHA)

	repo, path, pathErr := cache.path("example")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	_ = cache.receiveTransaction(context.Background(), repo, path, "diag-actor",
		func() error {
			return cache.updateRef(context.Background(), path, "refs/heads/feature/pending-diag", featureSHA, "")
		}, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return nil }))

	pending, err := cache.PendingReceives()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want 1", pending)
	}
	if pending[0].State != receiveGateFailed {
		t.Fatalf("pending state = %q, want %q", pending[0].State, receiveGateFailed)
	}
	if pending[0].Actor != "diag-actor" {
		t.Fatalf("pending actor = %q, want diag-actor", pending[0].Actor)
	}
	if pending[0].Repo != "example" {
		t.Fatalf("pending repo = %q, want example", pending[0].Repo)
	}
}

// TestReservedReplayRefusesWhileGateFails verifies that a reserved-state
// reservation (simulated crash before the gate ran) is not finalized when the
// gate is still failing at replay time. The reservation must persist in its
// original state and the handler must never be called.
func TestReservedReplayRefusesWhileGateFails(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	upstreamFunc := func(repo string) (string, error) {
		if repo != "example" {
			return "", fmt.Errorf("unknown repo %s", repo)
		}
		return repository.upstream, nil
	}
	cache := newTestCacheAt(t, root, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	featureSHA := repository.featureCommit(t, "feature/reserved-gate-fail", "reserved gate fail\n")
	seedCacheObject(t, repository.work, ready.Path, featureSHA)

	repo, path, pathErr := cache.path("example")
	if pathErr != nil {
		t.Fatal(pathErr)
	}

	// Create a reserved-state reservation — simulate crash after receive-pack
	// applied the ref update but before the gate was ever called.
	lock := cache.repoLock(repo)
	lock.Lock()
	if _, err := cache.reserveReceiveLocked(context.Background(), repo, path, "reserved-actor", ReleaseAdmission{}); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	if err := cache.updateRef(context.Background(), path, "refs/heads/feature/reserved-gate-fail", featureSHA, ""); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock()

	// Verify the on-disk reservation is in reserved state.
	reservation, readErr := cache.readReservation(repo)
	if readErr != nil {
		t.Fatalf("reservation should exist: %v", readErr)
	}
	if reservation.State != receiveReserved {
		t.Fatalf("reservation state = %q, want %q", reservation.State, receiveReserved)
	}

	// Restart with a failing gate — replay must refuse finalization.
	restarted, err := New(Config{
		Root:           root,
		CommandTimeout: 10 * time.Second,
		Upstream:       upstreamFunc,
		PreFinalizeGate: func(context.Context) error {
			return errors.New("gate check failed for reserved")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReplayPending(context.Background(), ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error {
		t.Fatal("handler should not be called while gate fails for reserved reservation")
		return nil
	})); err == nil {
		t.Fatal("replay should fail while gate is failing for reserved reservation")
	}
	// Reservation must still exist in reserved state.
	reservation, readErr = restarted.readReservation(repo)
	if readErr != nil {
		t.Fatalf("reservation should persist after failed replay: %v", readErr)
	}
	if reservation.State != receiveReserved {
		t.Fatalf("reservation state = %q, want %q", reservation.State, receiveReserved)
	}
	if reservation.Actor != "reserved-actor" {
		t.Fatalf("reservation actor = %q, want reserved-actor", reservation.Actor)
	}
	// Ref still visible — receive-pack applied it before the simulated crash.
	if got := runGitOptional(t, path, "show-ref", "--verify", "refs/heads/feature/reserved-gate-fail"); got == "" {
		t.Fatal("ref should be visible despite gate failure on reserved replay")
	}
}

// TestReservedReplayDeliversWithOriginalAttribution verifies that a
// reserved-state reservation (simulated crash before the gate ran) finalizes
// correctly when the gate is healthy at replay time: the gate runs, callbacks
// fire with the original actor, and the reservation is consumed.
func TestReservedReplayDeliversWithOriginalAttribution(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	upstreamFunc := func(repo string) (string, error) {
		if repo != "example" {
			return "", fmt.Errorf("unknown repo %s", repo)
		}
		return repository.upstream, nil
	}
	cache := newTestCacheAt(t, root, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	featureSHA := repository.featureCommit(t, "feature/reserved-recover", "reserved recover\n")
	seedCacheObject(t, repository.work, ready.Path, featureSHA)

	repo, path, pathErr := cache.path("example")
	if pathErr != nil {
		t.Fatal(pathErr)
	}

	// Create a reserved-state reservation — simulate crash before gate ran.
	lock := cache.repoLock(repo)
	lock.Lock()
	if _, err := cache.reserveReceiveLocked(context.Background(), repo, path, "original-reserved-actor", ReleaseAdmission{}); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	if err := cache.updateRef(context.Background(), path, "refs/heads/feature/reserved-recover", featureSHA, ""); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock()

	// Restart with a healthy gate — the gate must run and the replay must
	// finalize with the original actor attribution.
	var gateRan bool
	restarted, err := New(Config{
		Root:           root,
		CommandTimeout: 10 * time.Second,
		Upstream:       upstreamFunc,
		PreFinalizeGate: func(context.Context) error {
			gateRan = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var replayed []ReceiveEvent
	if err := restarted.ReplayPending(context.Background(), ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		replayed = append(replayed, event)
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if !gateRan {
		t.Fatal("pre-finalize gate should have been called for reserved reservation")
	}
	if len(replayed) != 1 {
		t.Fatalf("replayed %d events, want 1", len(replayed))
	}
	if replayed[0].Actor != "original-reserved-actor" {
		t.Fatalf("replayed actor = %q, want original-reserved-actor", replayed[0].Actor)
	}
	if replayed[0].Update.Ref != "refs/heads/feature/reserved-recover" || replayed[0].Update.NewSHA != featureSHA {
		t.Fatalf("replayed event = %+v", replayed[0])
	}
	// Reservation consumed after successful replay.
	if pending, err := restarted.PendingReceives(); err != nil || len(pending) != 0 {
		t.Fatalf("pending after recovery = %+v, %v", pending, err)
	}
	// Ref still visible.
	if got := runGitOptional(t, path, "show-ref", "--verify", "refs/heads/feature/reserved-recover"); got == "" {
		t.Fatal("ref should still be visible after gate recovery")
	}
}

func newTestCacheAt(t *testing.T, root, upstream string) *Cache {
	t.Helper()
	cache, err := New(Config{
		Root:           root,
		CommandTimeout: 10 * time.Second,
		Upstream: func(repo string) (string, error) {
			if repo != "example" {
				return "", fmt.Errorf("unknown repo %s", repo)
			}
			return upstream, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

// --- Test helpers for public-ref cap tests ---

var zeroObjectID = strings.Repeat("0", 40)

type receiveCommand struct {
	oldSHA string
	newSHA string
	ref    string
}

func receiveRawCommands(cache *Cache, actor string, commands []receiveCommand, handler ReceiveHandler) (string, error) {
	var input bytes.Buffer
	for index, command := range commands {
		line := fmt.Sprintf("%s %s %s", command.oldSHA, command.newSHA, command.ref)
		if index == 0 {
			line += "\x00report-status"
		}
		line += "\n"
		_, _ = fmt.Fprintf(&input, "%04x%s", len(line)+4, line)
	}
	_, _ = input.WriteString("0000")
	emptyPack := []byte{'P', 'A', 'C', 'K', 0, 0, 0, 2, 0, 0, 0, 0}
	checksum := sha1.Sum(emptyPack)
	_, _ = input.Write(emptyPack)
	_, _ = input.Write(checksum[:])
	var output bytes.Buffer
	err := cache.Receive(
		context.Background(), "example", actor, "", &input, &output, &output, handler,
	)
	return output.String(), err
}

func receiveDiagnostic(output string) string {
	const rejection = "oberth: rejected receive:"
	if start := strings.Index(output, rejection); start >= 0 {
		if end := strings.IndexByte(output[start:], '\n'); end >= 0 {
			return output[start : start+end]
		}
		return output[start:]
	}
	const maximum = 1024
	if len(output) > maximum {
		return "..." + output[len(output)-maximum:]
	}
	return output
}

func seedPublicBranches(t *testing.T, path, prefix string, count int, sha string) []string {
	t.Helper()
	return seedRefs(t, path, "refs/heads/"+prefix, count, sha)
}

func seedRefs(t *testing.T, path, prefix string, count int, sha string) []string {
	t.Helper()
	refs := make([]string, 0, count)
	var input strings.Builder
	for index := range count {
		ref := fmt.Sprintf("%s/%04d", prefix, index)
		refs = append(refs, ref)
		_, _ = fmt.Fprintf(&input, "create %s %s\n", ref, sha)
	}
	command := exec.Command("git", "update-ref", "--stdin")
	command.Dir = path
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	command.Stdin = strings.NewReader(input.String())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("seed %d refs below %s: %v\n%s", count, prefix, err, output)
	}
	return refs
}

// --- Receive cap tests ---

func TestReceivePackAcceptsMaximumWorkAndReplaysItBoundedAfterRestart(t *testing.T) {
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	cache := newTestCacheAt(t, root, repository.upstream)
	if _, err := cache.Ensure(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	commands := make([]receiveCommand, 0, maximumReceiveRefUpdates)
	for index := range maximumReceiveRefUpdates {
		commands = append(commands, receiveCommand{
			oldSHA: zeroObjectID,
			newSHA: repository.initialSHA,
			ref:    fmt.Sprintf("refs/heads/max/%04d", index),
		})
	}
	var events []ReceiveEvent
	output, err := receiveRawCommands(cache, "maximum-actor", commands, ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		events = append(events, event)
		return nil
	}))
	if err != nil {
		t.Fatalf("maximum work receive: %v\n%s", err, receiveDiagnostic(output))
	}
	if len(events) != maximumReceiveRefUpdates {
		t.Fatalf("events = %d, want %d", len(events), maximumReceiveRefUpdates)
	}
}

func TestReceivePackRejectsUpdateCapPlusOneWithoutPartialPublication(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	featureSHA := repository.featureCommit(t, "feature/over-cap", "over cap\n")
	deletions := seedPublicBranches(t, ready.Path, "delete", 85, repository.initialSHA)
	replacements := seedPublicBranches(t, ready.Path, "replace", 86, repository.initialSHA)

	commands := make([]receiveCommand, 0, maximumReceiveRefUpdates+1)
	for index := range 86 {
		commands = append(commands, receiveCommand{
			oldSHA: zeroObjectID,
			newSHA: repository.initialSHA,
			ref:    fmt.Sprintf("refs/heads/new/%04d", index),
		})
	}
	for _, ref := range deletions {
		commands = append(commands, receiveCommand{oldSHA: repository.initialSHA, newSHA: zeroObjectID, ref: ref})
	}
	for _, ref := range replacements {
		commands = append(commands, receiveCommand{oldSHA: repository.initialSHA, newSHA: featureSHA, ref: ref})
	}

	callbackCount := 0
	output, receiveErr := receiveRawCommands(cache, "over-limit-actor", commands, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error {
		callbackCount++
		return nil
	}))
	if receiveErr != nil || callbackCount != 0 {
		t.Fatalf("over-limit receive = %v, callbacks=%d; want pre-receive hook rejection\n%s", receiveErr, callbackCount, receiveDiagnostic(output))
	}
	if !strings.Contains(output, fmt.Sprintf("receive exceeds %d ref updates", maximumReceiveRefUpdates)) {
		t.Fatalf("over-limit rejection message missing: %s", receiveDiagnostic(output))
	}
}

func TestReceivePackRepeatedSmallPushesStopAtPublicRefCap(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	// Seed to 3 below cap (main already counts as 1).
	seedPublicBranches(t, ready.Path, "existing", maximumPublicRefs-3, repository.initialSHA)

	handler := ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return nil })
	// Push 2 more, reaching exactly 4096.
	for i := range 2 {
		command := receiveCommand{
			oldSHA: zeroObjectID,
			newSHA: repository.initialSHA,
			ref:    fmt.Sprintf("refs/heads/repeated/%04d", i),
		}
		if output, err := receiveRawCommands(cache, "small-push-actor", []receiveCommand{command}, handler); err != nil {
			t.Fatalf("push %d to reach cap: %v\n%s", i, err, receiveDiagnostic(output))
		}
	}

	// The 4097th ref must be rejected.
	rejected := receiveCommand{oldSHA: zeroObjectID, newSHA: repository.initialSHA, ref: "refs/heads/repeated/rejected"}
	output, receiveErr := receiveRawCommands(cache, "small-push-actor", []receiveCommand{rejected}, handler)
	if receiveErr != nil {
		t.Fatalf("rejected push returned Go error: %v", receiveErr)
	}
	if !strings.Contains(output, fmt.Sprintf("would leave %d public refs; limit is %d", maximumPublicRefs+1, maximumPublicRefs)) {
		t.Fatalf("cap rejection message missing: %s", receiveDiagnostic(output))
	}
	if got := runGitOptional(t, ready.Path, "show-ref", "--verify", "refs/heads/repeated/rejected"); got != "" {
		t.Fatal("rejected ref was created despite cap")
	}
}

func TestReceivePackPublicCapAllowsDeleteCreateReplacementAndIgnoresServerRefs(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	public := seedPublicBranches(t, ready.Path, "at-cap", maximumPublicRefs-1, repository.initialSHA)

	// Delete one + create one = net zero delta at the cap.
	commands := []receiveCommand{
		{oldSHA: repository.initialSHA, newSHA: zeroObjectID, ref: public[0]},
		{oldSHA: zeroObjectID, newSHA: repository.initialSHA, ref: "refs/heads/at-cap-replacement"},
	}
	if output, err := receiveRawCommands(cache, "replacement-actor", commands, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error {
		return nil
	})); err != nil {
		t.Fatalf("delete+create at cap: %v\n%s", err, receiveDiagnostic(output))
	}
	if got := runGitOptional(t, ready.Path, "show-ref", "--verify", public[0]); got != "" {
		t.Fatal("deleted ref survived")
	}
	if got := runGitOptional(t, ready.Path, "show-ref", "--verify", "refs/heads/at-cap-replacement"); got == "" {
		t.Fatal("replacement ref was not created")
	}
}

func TestReceivePackRecoversMaximumLegacyPublicNamespaceWithBranchDeletions(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	legacy := seedPublicBranches(t, ready.Path, "legacy-over-cap", maximumReceiveSnapshotRefs-1, repository.initialSHA)

	var events []ReceiveEvent
	commands := make([]receiveCommand, 0, maximumReceiveRefUpdates)
	for _, ref := range legacy[:maximumReceiveRefUpdates] {
		commands = append(commands, receiveCommand{oldSHA: repository.initialSHA, newSHA: zeroObjectID, ref: ref})
	}
	if output, err := receiveRawCommands(cache, "legacy-recovery-actor", commands, ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		events = append(events, event)
		return nil
	})); err != nil {
		t.Fatalf("legacy deletion recovery: %v\n%s", err, receiveDiagnostic(output))
	}
	if len(events) != maximumReceiveRefUpdates {
		t.Fatalf("legacy deletion event count = %d, want %d", len(events), maximumReceiveRefUpdates)
	}
	for _, event := range events {
		if event.Actor != "legacy-recovery-actor" || !event.Update.Deleted() || event.Update.Kind() != "branch" {
			t.Fatalf("legacy deletion event = %+v", event)
		}
	}
	after, err := cache.SnapshotRefs(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != maximumPublicRefs {
		t.Fatalf("public refs after recovery = %d, want %d", len(after), maximumPublicRefs)
	}
}

func TestReceivePackRejectsCreationWhileLegacyPublicNamespaceIsAboveCap(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	seedPublicBranches(t, ready.Path, "legacy-creation", maximumPublicRefs, repository.initialSHA)

	callbackCount := 0
	command := receiveCommand{oldSHA: zeroObjectID, newSHA: repository.initialSHA, ref: "refs/heads/legacy-creation-rejected"}
	output, receiveErr := receiveRawCommands(cache, "legacy-creation-actor", []receiveCommand{command}, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error {
		callbackCount++
		return nil
	}))
	if receiveErr != nil || !strings.Contains(output, "recovery receive may contain only exact-current branch deletions") {
		t.Fatalf("legacy creation receive = %v\n%s", receiveErr, receiveDiagnostic(output))
	}
	if callbackCount != 0 {
		t.Fatalf("rejected legacy creation delivered %d callbacks", callbackCount)
	}
}
