package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type deletionAuditor struct {
	actions []model.AuditActionSpec
	err     error
}

func (a *deletionAuditor) AppendAuditAction(_ context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	a.actions = append(a.actions, spec)
	return model.AuditAction{}, a.err
}

type failingReceiveRecorder struct{ err error }

func (recorder failingReceiveRecorder) RecordReceiveEvent(context.Context, model.ReceiveEventSpec) (bool, error) {
	return false, recorder.err
}

// A branch deletion receipt is one durable receive event (its transaction
// also holds the pending upstream deletion). The receive returns without any
// upstream call, and the propagator is woken.
func TestBranchDeletionRecordsPendingUpstreamDeletionAndReturns(t *testing.T) {
	git := &stubPushGit{}
	receives := &stubReceiveRecorder{}
	woken := 0
	ingestor, err := NewPushIngestor(PushConfig{
		Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: "main"}},
		Discoverer:   stubRepositoryDiscoverer{}, Git: git, Receives: receives, DeletionQueued: func() { woken++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ingestor.Branch(t.Context(), BranchPush{EventID: "delete", Repository: "repo", Branch: "feature/retired",
		Actor: "operator@host", OldOID: strings.Repeat("a", 40), NewOID: strings.Repeat("0", 40)})
	if err != nil || !result.Deleted || !strings.Contains(result.Reason, "upstream deletion queued") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(receives.events) != 1 || receives.events[0].Outcome != "deleted" || receives.events[0].OldSHA != strings.Repeat("a", 40) || receives.events[0].Actor != "operator@host" {
		t.Fatalf("events = %+v", receives.events)
	}
	if len(git.deletedBranches) != 1 || woken != 1 || git.ensureCalls != 0 {
		t.Fatalf("marker calls=%v woken=%d ensure=%d (a deletion must not refresh from the forge)", git.deletedBranches, woken, git.ensureCalls)
	}
}

// The marker is best effort: a failed or slow local marker never fails the
// receive, while a failed durable record does.
func TestBranchDeletionDependsOnlyOnTheDurableRecord(t *testing.T) {
	git := &stubPushGit{deleteErr: errors.New("repository lock busy")}
	ingestor, err := NewPushIngestor(PushConfig{
		Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: "main"}},
		Discoverer:   stubRepositoryDiscoverer{}, Git: git, Receives: &stubReceiveRecorder{},
	})
	if err != nil {
		t.Fatal(err)
	}
	push := BranchPush{EventID: "delete", Repository: "repo", Branch: "feature/retired",
		Actor: "operator@host", OldOID: strings.Repeat("a", 40), NewOID: strings.Repeat("0", 40)}
	if result, err := ingestor.Branch(t.Context(), push); err != nil || !result.Deleted {
		t.Fatalf("marker failure failed the receive: %+v %v", result, err)
	}
	failing, err := NewPushIngestor(PushConfig{
		Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: "main"}},
		Discoverer:   stubRepositoryDiscoverer{}, Git: &stubPushGit{}, Receives: failingReceiveRecorder{err: errors.New("database unavailable")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := failing.Branch(t.Context(), push); err == nil || result.Deleted {
		t.Fatalf("unrecorded deletion acknowledged: %+v %v", result, err)
	}
}

func TestBranchDeletionRefusesDefaultBranch(t *testing.T) {
	git := &stubPushGit{}
	receives := &stubReceiveRecorder{}
	ingestor, err := NewPushIngestor(PushConfig{
		Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: "main"}},
		Discoverer:   stubRepositoryDiscoverer{}, Git: git, Receives: receives,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ingestor.Branch(t.Context(), BranchPush{EventID: "delete", Repository: "repo", Branch: "main",
		Actor: "agent@host", OldOID: strings.Repeat("a", 40), NewOID: strings.Repeat("0", 40)})
	if err == nil || !strings.Contains(err.Error(), "default branch") || len(git.deletedBranches) != 0 || len(receives.events) != 0 {
		t.Fatalf("default deletion: err=%v calls=%v events=%v", err, git.deletedBranches, receives.events)
	}
}
