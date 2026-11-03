package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPriorityReviewRequestsOnlyRaiseAutomaticClass(t *testing.T) {
	for _, tc := range []struct {
		name, ref, trigger string
		kind               model.RefKind
		request            model.RunPriority
		admin              bool
		want               model.RunPriority
		reason             string
		refused            bool
	}{
		{"ungranted promotion", "promotion/main", "promotion", model.RefBranch, model.PriorityHigh, false, model.PriorityHighest, "promotion run", true},
		{"admin high promotion", "promotion/main", "promotion", model.RefBranch, model.PriorityHigh, true, model.PriorityHighest, "promotion run", false},
		{"typo RC", "rc/v1", "rc", model.RefBranch, "hgh", false, model.PriorityHigh, "release-candidate pattern: rc/*", true},
		{"typo tag", "v1", "tag", model.RefTag, "hgh", false, model.PriorityHighest, model.TagReleasePriorityReason, true},
		{"normal RC", "rc/v1", "branch", model.RefBranch, model.PriorityNormal, false, model.PriorityHigh, "release-candidate pattern: rc/*", false},
		{"normal tag", "v1", "tag", model.RefTag, model.PriorityNormal, false, model.PriorityHighest, model.TagReleasePriorityReason, false},
		{"invalid reason RC", "rc/v1", "branch", model.RefBranch, model.PriorityHighest, true, model.PriorityHigh, "release-candidate pattern: rc/*", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			if _, err := s.SetQueuePriorityPatterns(t.Context(), "admin", []string{"rc/*"}); err != nil {
				t.Fatal(err)
			}
			spec := testRunSpec(repo.ID, tc.ref, strings.Repeat("a", 40))
			spec.RefKind, spec.Trigger = tc.kind, tc.trigger
			reason := "urgent"
			if tc.name == "invalid reason RC" {
				reason = "bad\nreason"
			}
			spec.PriorityRequest = model.PriorityRequest{Priority: tc.request, PriorityReason: reason, UnscopedAdmin: &tc.admin}
			result, err := s.EnqueueRun(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Run(t.Context(), result.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Priority != tc.want || got.PriorityReason != tc.reason || (got.PriorityRefusal != "") != tc.refused || !got.PriorityProtected {
				t.Fatalf("priority=%s reason=%q refusal=%q protected=%v; want %s/%q refusal=%v", got.Priority, got.PriorityReason, got.PriorityRefusal, got.PriorityProtected, tc.want, tc.reason, tc.refused)
			}
		})
	}
}
