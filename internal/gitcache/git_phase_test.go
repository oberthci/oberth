package gitcache

import (
	"context"
	"errors"
	"testing"
)

func TestUnknownTransactionPhaseNamesPackagedGit(t *testing.T) {
	t.Parallel()
	cache, session, _ := gateFixture(t)
	held := 0
	err := cache.gateOperation(t.Context(), session, gateRequest{Phase: "future"}, &held)
	var refusal *RefusalError
	if !errors.As(err, &refusal) {
		t.Fatal("unknown phase did not return a public refusal", err)
	}
	if refusal.Reason() != "server Git reported an unknown phase" || refusal.Remedy() != "Use the packaged Git for this server." {
		t.Fatal("unknown phase gave source-input advice", refusal)
	}
}

func TestGitPreparationPhasesKeepThePublicationLockAtPrepared(t *testing.T) {
	t.Parallel()
	cache, session, _ := gateFixture(t)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	progress := watchProgress(cancel, 0)
	defer progress.close()
	session.progress = &receiveProgress{progress: progress}
	transport := 0
	record, err := readReservation(session.store)
	if err != nil {
		t.Fatal(err)
	}
	record.State = "ready"
	if err = writeReservation(session.store, record); err != nil {
		t.Fatal(err)
	}
	session.progress.begin = func() { transport++ }
	session.progress.end = func() { transport++ }
	if err := cache.lock(ctx, session.store); err != nil {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	held := 0
	err = cache.gateOperation(canceled, session, gateRequest{Phase: "preparing"}, &held)
	cache.unlock(session.store)
	if err != nil || [2]int{held, transport} != [2]int{0, 1} {
		t.Fatalf("preparing took a lock or kept transport active: %v %d %d", err, held, transport)
	}
	for _, phase := range []string{"prepared", "prepared", "committed", "aborted"} {
		if err = cache.gateOperation(ctx, session, gateRequest{Phase: phase}, &held); err != nil {
			t.Fatal(phase, err)
		}
	}
	record, err = readReservation(session.store)
	if err != nil || record.Prepared == nil || [2]int{held, transport} != [2]int{0, 1} {
		t.Fatal("unbalanced or unrecorded native transaction", held, record.Prepared, transport, err)
	}
	if err = cache.lock(ctx, session.store); err != nil {
		t.Fatal("transaction retained the publication lock", err)
	}
	cache.unlock(session.store)
}
