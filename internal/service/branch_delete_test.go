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
