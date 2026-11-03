package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type deletionAuditor struct {
	actions   []model.AuditActionSpec
	err       error
	errAction string
}

func (a *deletionAuditor) AppendAuditAction(_ context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	a.actions = append(a.actions, spec)
	if a.errAction != "" && a.errAction != spec.Action {
		return model.AuditAction{}, nil
	}
	return model.AuditAction{}, a.err
}

func TestBranchDeletionRecordsOnlySuccessfulAuditedOutcome(t *testing.T) {
	for _, failure := range []string{"none", "missing auditor", "intent", "mutation", "outcome"} {
		t.Run(failure, func(t *testing.T) {
			git := &stubPushGit{}
			receives := &stubReceiveRecorder{}
			audit := &deletionAuditor{}
			var auditor Auditor = audit
			switch failure {
			case "missing auditor":
				auditor = nil
			case "intent", "outcome":
				audit.err = errors.New("audit unavailable")
				if failure == "outcome" {
					audit.errAction = "branch.delete.succeeded"
				}
			case "mutation":
				git.deleteErr = errors.New("upstream unavailable")
			}
			ingestor, err := NewPushIngestor(PushConfig{
				Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: "main"}},
				Discoverer:   stubRepositoryDiscoverer{}, Git: git, Receives: receives, Auditor: auditor,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := ingestor.Branch(t.Context(), BranchPush{EventID: "delete", Repository: "repo", Branch: "feature/retired",
				Actor: "operator@host", OldOID: strings.Repeat("a", 40), NewOID: strings.Repeat("0", 40)})
			if failure != "none" {
				if err == nil || result.Deleted || len(receives.events) != 0 {
					t.Fatalf("failed deletion recorded as deleted: result=%+v events=%v err=%v", result, receives.events, err)
				}
				return
			}
			if err != nil || !result.Deleted || len(receives.events) != 1 || receives.events[0].Outcome != "deleted" {
				t.Fatalf("successful audited deletion not recorded: result=%+v events=%v err=%v", result, receives.events, err)
			}
		})
	}
}

func TestBranchDeletionAuditsActorAndOldSHA(t *testing.T) {
	for _, rejectAudit := range []bool{false, true} {
		git := &stubPushGit{}
		audit := &deletionAuditor{}
		if rejectAudit {
			audit.err = errors.New("audit failed")
		}
		ingestor, err := NewPushIngestor(PushConfig{
			Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: "main"}},
			Discoverer:   stubRepositoryDiscoverer{}, Git: git, Receives: &stubReceiveRecorder{}, Auditor: audit,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = ingestor.Branch(t.Context(), BranchPush{EventID: "delete", Repository: "repo", Branch: "feature/retired",
			Actor: "operator@host", OldOID: strings.Repeat("a", 40), NewOID: strings.Repeat("0", 40)})
		if rejectAudit {
			if err == nil || len(git.deletedBranches) != 0 {
				t.Fatalf("unaudited delete: %v %v", err, git.deletedBranches)
			}
			continue
		}
		if err != nil || len(git.deletedBranches) != 1 || len(audit.actions) != 2 {
			t.Fatalf("delete err=%v git=%v audit=%v", err, git.deletedBranches, audit.actions)
		}
		for i, action := range []string{"branch.delete.intent", "branch.delete.succeeded"} {
			got := audit.actions[i]
			if got.Action != action || got.Actor != "operator@host" || !strings.Contains(got.Details, strings.Repeat("a", 40)) || !strings.Contains(got.Details, "feature/retired") {
				t.Fatalf("audit = %+v", got)
			}
		}
	}
}

func TestBranchDeletionFailsClosed(t *testing.T) {
	for _, branch := range []string{"main", "feature/unaudited"} {
		t.Run(branch, func(t *testing.T) {
			git := &stubPushGit{}
			receives := &stubReceiveRecorder{}
			ingestor, err := NewPushIngestor(PushConfig{
				Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: "main"}},
				Discoverer:   stubRepositoryDiscoverer{}, Git: git, Receives: receives,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = ingestor.Branch(t.Context(), BranchPush{EventID: "delete", Repository: "repo", Branch: branch,
				Actor: "agent@host", OldOID: strings.Repeat("a", 40), NewOID: strings.Repeat("0", 40)})
			if err == nil || len(git.deletedBranches) != 0 {
				t.Fatalf("unsafe deletion: err=%v calls=%v", err, git.deletedBranches)
			}
			if branch == "main" && (len(receives.events) != 0 || !strings.Contains(err.Error(), "default branch")) {
				t.Fatalf("default deletion recorded or unclear refusal: err=%v events=%v", err, receives.events)
			}
		})
	}
}
