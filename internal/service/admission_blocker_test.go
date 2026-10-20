package service

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func blockerRequest(runID string, repoID int64, repo, ref string, size periapsis.Size) admissionRequest {
	return admissionRequest{RunID: runID, RepoID: repoID, Repo: repo, Ref: ref, Size: size}
}

// TestAnXLHeadIsNamedByEveryRunBehindIt is #1021 part 2: with an XL run at the
// queue head waiting for a full drain, the runs behind it say what they wait
// for, the head says it is the head, and status names it.
func TestAnXLHeadIsNamedByEveryRunBehindIt(t *testing.T) {
	gate := newWeightedAdmissionForJobs(3)
	scheduler := &Scheduler{admission: gate}
	if scheduler.AdmissionBlocker() != nil {
		t.Fatal("an empty gate reports a blocker")
	}
	releaseRunning := acquireRequestWithinBudget(t, gate.reserve(1), blockerRequest("running", 1, "docs", "main", periapsis.M))
	head := gate.reserve(2)
	headReady := make(chan func(), 1)
	go acquireRequestForTest(head, blockerRequest("ebpf-run", 2, "cloudtaser-ebpf", "promotion/main/335d751c79f3", periapsis.XL), headReady)
	awaitWaiting(t, head)
	behind := gate.reserve(3)
	behindReady := make(chan func(), 1)
	go acquireRequestForTest(behind, blockerRequest("small", 3, "website", "feature/x", periapsis.S), behindReady)
	awaitWaiting(t, behind)

	const want = "blocked behind cloudtaser-ebpf/promotion/main/335d751c79f3 (XL, needs a full drain)."
	snapshot := scheduler.AdmissionSnapshot(3)
	if snapshot.BlockedBy == nil || snapshot.BlockedBy.RunID != "ebpf-run" || !snapshot.BlockedBy.FullDrain || snapshot.HeadOfQueue {
		t.Fatalf("behind snapshot = %#v", snapshot)
	}
	if message := waitingAdmissionMessage(snapshot); !strings.HasSuffix(message, want) {
		t.Fatalf("behind message = %q", message)
	}
	headSnapshot := scheduler.AdmissionSnapshot(2)
	if !headSnapshot.HeadOfQueue || headSnapshot.BlockedBy == nil || headSnapshot.BlockedBy.RunID != "ebpf-run" {
		t.Fatalf("head snapshot = %#v", headSnapshot)
	}
	if message := waitingAdmissionMessage(headSnapshot); !strings.Contains(message, "At the head") || !strings.Contains(message, "XL, needs a full drain") {
		t.Fatalf("head message = %q", message)
	}
	blocker := scheduler.AdmissionBlocker()
	if blocker == nil || blocker.Repo != "cloudtaser-ebpf" || blocker.Used != 2 || blocker.Capacity != 6 || blocker.WaitingSince.IsZero() {
		t.Fatalf("status blocker = %#v", blocker)
	}

	// Unclaimed queued runs carry the same reason, and a scoped actor that
	// cannot see the blocking repository does not learn about it.
	api := &API{admission: scheduler}
	queued := api.scheduling(context.Background(), model.Run{ID: "q", Status: model.RunQueued}, nil)
	if !strings.HasSuffix(queued.Message, want) {
		t.Fatalf("queued message = %q", queued.Message)
	}
	scoped := &API{admission: scheduler, scope: &repoScope{repos: map[int64]model.Repository{3: {ID: 3}}}}
	if message := scoped.scheduling(context.Background(), model.Run{ID: "q", Status: model.RunQueued}, nil).Message; strings.Contains(message, "cloudtaser-ebpf") {
		t.Fatalf("scoped queued message leaks the blocker: %q", message)
	}
	visible := &API{admission: scheduler, scope: &repoScope{repos: map[int64]model.Repository{2: {ID: 2}, 3: {ID: 3}}}}
	if message := visible.scheduling(context.Background(), model.Run{ID: "w", Status: model.RunRunning, QueueSequence: 3}, nil).Message; !strings.HasSuffix(message, want) {
		t.Fatalf("in-scope waiting message = %q", message)
	}

	releaseRunning()
	releaseHead := awaitAcquired(t, headReady)
	if scheduler.AdmissionBlocker().RunID != "small" {
		t.Fatal("the S run behind the admitted XL is not the new head")
	}
	releaseHead()
	awaitAcquired(t, behindReady)()
	if scheduler.AdmissionBlocker() != nil {
		t.Fatal("a drained gate still reports a blocker")
	}
}
