package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

type stalledParkPolicyReader struct {
	*parkPushGit
	entered, deadline time.Time
	readErr           error
}

func (g *stalledParkPolicyReader) ReadDefaultBlob(ctx context.Context, _ string, _ string, _ int) (string, []byte, bool, error) {
	g.entered = time.Now()
	var bounded bool
	g.deadline, bounded = ctx.Deadline()
	if !bounded || g.deadline.After(g.entered.Add(2*time.Second)) {
		// Refuse a missing/expanded budget without hanging the test.
		return "", nil, false, context.Canceled
	}
	<-ctx.Done()
	g.readErr = ctx.Err()
	return "", nil, false, g.readErr
}

func TestParkPushPolicyDeadlineFallsBackToCI(t *testing.T) {
	sha := strings.Repeat("a", 40)
	g := &stalledParkPolicyReader{parkPushGit: &parkPushGit{stubPushGit: &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: sha, CommitSHA: sha}}}}
	queue := &stubCIQueue{}
	p, err := NewPushIngestor(PushConfig{Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: "main"}}, Discoverer: stubRepositoryDiscoverer{}, Git: g, Receives: &stubReceiveRecorder{}, Branches: queue})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := p.Branch(t.Context(), BranchPush{EventID: "policy-timeout", Repository: "repo", Branch: "park/review", NewOID: sha, Actor: "dev"})
	if err != nil || len(g.mirrored) != 0 || len(queue.requests) != 1 {
		t.Fatalf("deadline bypassed ordinary CI: result=%+v mirrored=%v queued=%v err=%v", result, g.mirrored, queue.requests, err)
	}
	// Bracket deadline construction, rather than placing an elapsed-time upper
	// bound on a scheduler that may be busy. The actual reader waits for Done.
	if g.deadline.Before(started.Add(2*time.Second)) || g.deadline.After(g.entered.Add(2*time.Second)) || !errors.Is(g.readErr, context.DeadlineExceeded) {
		t.Fatalf("production two-second deadline changed: started=%v entered=%v deadline=%v error=%v", started, g.entered, g.deadline, g.readErr)
	}
	if got, want := queue.requests[0].Reason, "branch CI: park policy unavailable (read promotion policy: context deadline exceeded)"; got != want {
		t.Fatalf("deadline fallback reason=%q, want %q", got, want)
	}
}
