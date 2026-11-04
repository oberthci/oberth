package service

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

func TestPushPolicyOutageFallsBackToBranchCI(t *testing.T) {
	for _, branch := range []string{"feature/change", "park/review"} {
		t.Run(branch, func(t *testing.T) {
			sha := strings.Repeat("a", 40)
			g := &parkPushGit{stubPushGit: &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: sha, CommitSHA: sha}}, policyErr: context.DeadlineExceeded}
			queue, receipts := &stubCIQueue{}, &stubReceiveRecorder{}
			p, err := NewPushIngestor(PushConfig{Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: "main"}}, Discoverer: stubRepositoryDiscoverer{}, Git: g, Receives: receipts, Branches: queue, Auditor: &deletionAuditor{}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := p.Branch(t.Context(), BranchPush{EventID: "outage", Repository: "repo", Branch: branch, NewOID: sha, Actor: "pusher"})
			if err != nil || result.Run == nil || len(queue.requests) != 1 || len(g.mirrored) != 0 {
				t.Fatalf("policy outage stalled CI: %+v %v queue=%+v", result, err, queue.requests)
			}
		})
	}
}
