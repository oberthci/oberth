package service

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestPriorityReleaseOversizedTagKeepsAcceptedRequest(t *testing.T) {
	for _, requested := range []model.RunPriority{model.PriorityHigh, model.PriorityHighest, model.PriorityNormal} {
		for _, granted := range []bool{false, true} {
			name := string(requested) + "/refused"
			if granted {
				name = string(requested) + "/granted"
			}
			t.Run(name, func(t *testing.T) {
				f := newControlFixture(t)
				ctx := t.Context()
				if granted {
					if _, err := f.store.SetPriorityGrant(ctx, "admin", model.PriorityGrant{Identity: "requester", MaxClass: model.PriorityHighest, Expires: time.Now().Add(time.Hour)}); err != nil {
						t.Fatal(err)
					}
				}
				tag, err := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefTag, Ref: "v1", SHA: strings.Repeat("a", 40), Actor: "requester", Trigger: "tag", PriorityRequest: model.PriorityRequest{Priority: requested, PriorityReason: "release blocker"}})
				if err != nil || tag.Priority != model.PriorityHighest {
					t.Fatalf("natural tag priority=%+v %v", tag, err)
				}
				g := newWeightedAdmission(4)
				f.scheduler.admission = g
				r := g.reserveRun(tag.Run)
				defer r.cancel()
				g.mu.Lock()
				r.prepared, r.size, r.priorityMaximum = true, periapsis.XL, periapsis.L
				g.mu.Unlock()
				want := model.PriorityNormal
				if granted && requested != model.PriorityNormal {
					want = requested
				}
				check := func(stage string) {
					t.Helper()
					p, reason, ok := f.scheduler.AdmissionPriority(tag.QueueSequence)
					if !ok || p != want || (want != model.PriorityNormal && reason != "submission priority: release blocker") {
						t.Errorf("%s priority=%s reason=%q found=%v, want %s", stage, p, reason, ok, want)
					}
				}
				check("reservation")
				if err := f.scheduler.refreshPriorities(ctx); err != nil {
					t.Fatal(err)
				}
				check("refresh")
				g.mu.Lock()
				r.priorityLookupFailed = true
				g.mu.Unlock()
				check("size lookup unavailable")
				// An override can predate the tag becoming its repository's oldest.
				g.mu.Lock()
				r.priorityOverride, r.priorityOverrideReason = model.PriorityHigh, "operator choice"
				g.mu.Unlock()
				if p, reason, _ := f.scheduler.AdmissionPriority(tag.QueueSequence); p != model.PriorityHigh || reason != "admin bump: operator choice" {
					t.Fatalf("override lost precedence: %s %q", p, reason)
				}
			})
		}
	}
}
