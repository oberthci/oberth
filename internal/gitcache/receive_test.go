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
